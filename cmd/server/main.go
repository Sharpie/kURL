package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bugsnag/bugsnag-go/v2"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"golang.org/x/net/publicsuffix"
)

const upstream = "http://localhost:3000"

func main() {
	log.Printf("Commit %s\n", os.Getenv("VERSION"))

	if bugsnagKey := os.Getenv("BUGSNAG_KEY"); bugsnagKey != "" {
		bugsnag.Configure(bugsnag.Configuration{
			APIKey:       bugsnagKey,
			ReleaseStage: os.Getenv("ENVIRONMENT"),
		})
	}

	r := mux.NewRouter()

	r.HandleFunc("/bundle/{installerID}", http.HandlerFunc(bundle))
	r.HandleFunc("/bundle/version/{kurlVersion}/{installerID}", http.HandlerFunc(bundle))

	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		log.Panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	r.PathPrefix("/").Handler(proxy)

	http.Handle("/", r)

	log.Println("Listening on :3001")
	err = http.ListenAndServe(":3001", bugsnag.Handler(nil))
	if err != nil {
		log.Fatal(err)
	}
}

type BundleManifest struct {
	Layers []string          `json:"layers"`
	Files  map[string]string `json:"files"`
	Images []string          `json:"images"`
}

var imagePolicy = []byte(`{
	"default": [{"type": "insecureAcceptAnything"}]
}`)

func bundle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Allow-Headers", "Access-Control-Allow-Origin, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("%s %s", r.Method, r.URL.Path)

	vars := mux.Vars(r)

	kurlVersion := vars["kurlVersion"]
	installerID := strings.TrimSuffix(vars["installerID"], ".tar.gz")
	var installerURL string
	if kurlVersion != "" {
		installerURL = fmt.Sprintf("%s/bundle/version/%s/%s", upstream, kurlVersion, installerID)
	} else {
		installerURL = fmt.Sprintf("%s/bundle/%s", upstream, installerID)
	}
	request, err := http.NewRequest("GET", installerURL, nil)
	if err != nil {
		err = errors.Wrapf(err, "error building request for %s", installerURL)
		handleHttpError(w, r, err, http.StatusInternalServerError)
		return
	}
	// forward request headers for metrics
	request.Header = r.Header
	if request.Header.Get("X-Forwarded-For") == "" {
		if host, _, _ := net.SplitHostPort(r.RemoteAddr); host != "" {
			request.Header.Set("X-Forwarded-For", host)
		}
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		err = errors.Wrapf(err, "error fetching %s", installerURL)
		handleHttpError(w, r, err, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		err = errors.Wrapf(err, "error reading response body from %s", installerURL)
		handleHttpError(w, r, err, http.StatusInternalServerError)
		return
	}
	if resp.StatusCode == http.StatusNotFound {
		http.Error(w, string(body), http.StatusNotFound)
		return
	}
	bundle := &BundleManifest{}
	err = json.Unmarshal(body, bundle)
	if err != nil {
		err = errors.Wrapf(err, "error unmarshaling installer bundle manifest from %s: %s", installerURL, body)
		handleHttpError(w, r, err, http.StatusInternalServerError)
		return
	}

	for _, srcURL := range bundle.Layers {
		resp, err := http.Head(srcURL)
		if err != nil {
			err = errors.Wrapf(err, "error http head %s for installer %s bundle", srcURL, installerID)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err := errors.Errorf("unexpected response status code %d", resp.StatusCode)
			err = errors.Wrapf(err, "error http head %s for installer %s bundle", srcURL, installerID)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
	}

	for _, image := range bundle.Images {
		if !allowRegistry(image) {
			err := errors.Errorf("Unsupported image registry %s", image)
			handleHttpError(w, r, err, http.StatusUnprocessableEntity)
			return
		}
	}

	w.Header().Set("Content-Type", "binary/octet-stream")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Transfer-Encoding", "chunked")

	if r.Method == "HEAD" {
		return
	}

	wz := gzip.NewWriter(w)
	archive := tar.NewWriter(wz)
	defer func() {
		// TODO: it would be better to somehow make this archive invalid if there is an error so
		// it's not just missing a package
		if err := archive.Close(); err != nil {
			err = errors.Wrapf(err, "error closing archive for installer %s", installerID)
			handleError(r.Context(), err)
		}

		if err := wz.Close(); err != nil {
			err = errors.Wrapf(err, "error closing gzip stream for installer %s", installerID)
			handleError(r.Context(), err)
		}
	}()

	for i, image := range bundle.Images {
		srcRef, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s", image))
		if err != nil {
			err = errors.Wrapf(err, "error parsing override image %q: %v", image, err)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}

		tempDir, err := ioutil.TempDir("/images", "temp-image-pull")
		if err != nil {
			err = errors.Wrap(err, "error creating temp directory")
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(tempDir)

		destPath := path.Join(tempDir, "temp-archive-image")
		destStr := fmt.Sprintf("docker-archive:%s:%s", destPath, image)
		localRef, err := alltransports.ParseImageName(destStr)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse local image name: %s", destStr)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}

		policy, err := signature.NewPolicyFromBytes(imagePolicy)
		if err != nil {
			err = errors.Wrap(err, "failed to read default image policy")
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		policyContext, err := signature.NewPolicyContext(policy)
		if err != nil {
			err = errors.Wrap(err, "failed to create image policy context")
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		destCtx := &types.SystemContext{
			DockerDisableV1Ping: true,
		}
		srcCtx := &types.SystemContext{
			DockerDisableV1Ping: true,
			AuthFilePath:        "/dev/null",
			DockerAuthConfig: &types.DockerAuthConfig{
				Username:      "",
				Password:      "",
				IdentityToken: "",
			},
		}
		_, err = copy.Image(r.Context(), policyContext, localRef, srcRef, &copy.Options{
			RemoveSignatures:      true,
			SignBy:                "",
			ForceManifestMIMEType: "",
			DestinationCtx:        destCtx,
			SourceCtx:             srcCtx,
		})
		if err != nil {
			err = errors.Wrapf(err, "failed to save docker image archive of %s", image)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}

		f, err := os.Open(destPath)
		if err != nil {
			err = errors.Wrap(err, "failed to open override image archive")
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		fi, err := f.Stat()
		if err != nil {
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}

		header := &tar.Header{
			Name:    fmt.Sprintf("kurl/image-overrides/%d.tar", i),
			Size:    fi.Size(),
			Mode:    0644,
			ModTime: time.Now(),
		}
		archive.WriteHeader(header)
		_, err = io.Copy(archive, f)
		if err != nil {
			err = errors.Wrapf(err, "copy file %s contents", header.Name)
			handleHttpError(w, r, err, http.StatusInternalServerError)
			return
		}
		os.RemoveAll(tempDir)
		runtime.GC()
	}

	for _, layerURL := range bundle.Layers {
		if err := pipe(archive, layerURL); err != nil {
			err = errors.Wrapf(err, "error piping %s to %s bundle", layerURL, installerID)
			handleError(r.Context(), err)
			return
		}
	}

	for filepath, contents := range bundle.Files {
		archive.WriteHeader(&tar.Header{
			Name:    filepath,
			Size:    int64(len(contents)),
			Mode:    0644,
			ModTime: time.Now(),
		})
		_, err := archive.Write([]byte(contents))
		if err != nil {
			err = errors.Wrapf(err, "error writing file %s for %s", filepath, installerID)
			handleError(r.Context(), err)
			return
		}
	}
}

func pipe(dst *tar.Writer, srcURL string) error {
	resp, err := http.Get(srcURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("unexpected response code %d", resp.StatusCode)
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return errors.Wrap(err, "gunzip response")
	}
	defer zr.Close()
	src := tar.NewReader(zr)

	for {
		header, err := src.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return errors.Wrap(err, "next file")
		}
		header.Name = filepath.Join("kurl", header.Name)
		dst.WriteHeader(header)
		_, err = io.Copy(dst, src)
		if err != nil {
			return errors.Wrapf(err, "copy file %s contents", header.Name)
		}
	}
}

func handleHttpError(w http.ResponseWriter, r *http.Request, err error, code int) {
	log.Println(err)
	http.Error(w, http.StatusText(code), code)
	bugsnag.Notify(err, r.Context())
}

func handleError(ctx context.Context, err error) {
	log.Println(err)
	bugsnag.Notify(err, ctx)
}

func allowRegistry(image string) bool {
	parsed, err := url.Parse(fmt.Sprintf("https://%s", image))
	if err != nil {
		return false
	}

	host, _ := publicsuffix.EffectiveTLDPlusOne(parsed.Hostname())

	switch host {
	case "docker.io", "gcr.io", "ghcr.io", "azurecr.io", "ttl.sh", "ecr.us-east-1.amazonaws.com":
		return true
	}

	return false
}
