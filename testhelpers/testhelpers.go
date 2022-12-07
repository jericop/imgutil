package testhelpers

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildpacks/imgutil/layer"

	dockertypes "github.com/docker/docker/api/types"
	dockercli "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/pkg/errors"
)

func RandString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(rand.Intn(26)) // #nosec G404
	}
	return string(b)
}

// Assert deep equality (and provide useful difference as a test failure)
func AssertEq(t *testing.T, actual, expected interface{}) {
	t.Helper()
	if diff := cmp.Diff(actual, expected); diff != "" {
		t.Fatal(diff)
	}
}

func AssertNotEq(t *testing.T, v1, v2 interface{}) {
	t.Helper()

	if diff := cmp.Diff(v1, v2); diff == "" {
		t.Fatalf("expected values not to be equal, both equal to %v", v1)
	}
}

func AssertContains(t *testing.T, slice []string, elements ...string) {
	t.Helper()

outer:
	for _, el := range elements {
		for _, actual := range slice {
			if diff := cmp.Diff(actual, el); diff == "" {
				continue outer
			}
		}

		t.Fatalf("Expected %+v to contain: %s", slice, el)
	}
}

func AssertDoesNotContain(t *testing.T, slice []string, elements ...string) {
	t.Helper()

	for _, el := range elements {
		for _, actual := range slice {
			if diff := cmp.Diff(actual, el); diff == "" {
				t.Fatalf("Expected %+v to NOT contain: %s", slice, el)
			}
		}
	}
}

func AssertMatch(t *testing.T, actual string, expected *regexp.Regexp) {
	t.Helper()
	if !expected.Match([]byte(actual)) {
		t.Fatal(cmp.Diff(actual, expected))
	}
}

func AssertError(t *testing.T, actual error, expected string) {
	t.Helper()
	if actual == nil {
		t.Fatalf("Expected an error but got nil")
	}
	if !strings.Contains(actual.Error(), expected) {
		t.Fatalf(
			`Expected error to contain "%s", got "%s"\n\n Diff:\n%s`,
			expected,
			actual.Error(),
			cmp.Diff(expected, actual.Error()),
		)
	}
}

func AssertNil(t *testing.T, actual interface{}) {
	t.Helper()
	if actual != nil {
		t.Fatalf("Expected nil: %s", actual)
	}
}

func AssertBlobsLen(t *testing.T, path string, expected int) {
	t.Helper()
	fis, err := os.ReadDir(filepath.Join(path, "blobs", "sha256"))
	AssertNil(t, err)
	AssertEq(t, len(fis), expected)
}

func AssertTrue(t *testing.T, p func() bool) {
	t.Helper()
	if !p() {
		t.Fatal("Expected predicate to be true")
	}
}

var dockerCliVal dockercli.CommonAPIClient
var dockerCliOnce sync.Once

func DockerCli(t *testing.T) dockercli.CommonAPIClient {
	dockerCliOnce.Do(func() {
		var dockerCliErr error
		dockerCliVal, dockerCliErr = dockercli.NewClientWithOpts(dockercli.FromEnv, dockercli.WithVersion("1.38"))
		AssertNil(t, dockerCliErr)
	})
	return dockerCliVal
}

func Eventually(t *testing.T, test func() bool, every time.Duration, timeout time.Duration) {
	t.Helper()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			if test() {
				return
			}
		case <-timer.C:
			t.Fatalf("timeout on eventually: %v", timeout)
		}
	}
}

func PullIfMissing(t *testing.T, docker dockercli.CommonAPIClient, ref string) {
	t.Helper()
	_, _, err := docker.ImageInspectWithRaw(context.TODO(), ref)
	if err == nil {
		return
	}
	if !dockercli.IsErrNotFound(err) {
		t.Fatalf("failed inspecting image '%s': %s", ref, err)
	}

	rc, err := docker.ImagePull(context.Background(), ref, dockertypes.ImagePullOptions{})
	if err != nil {
		// Retry
		rc, err = docker.ImagePull(context.Background(), ref, dockertypes.ImagePullOptions{})
		AssertNil(t, err)
	}
	defer rc.Close()

	AssertNil(t, checkResponseError(rc))

	_, err = io.Copy(ioutil.Discard, rc)
	AssertNil(t, err)
}

func DockerRmi(dockerCli dockercli.CommonAPIClient, repoNames ...string) error {
	var err error
	ctx := context.Background()
	for _, name := range repoNames {
		_, e := dockerCli.ImageRemove(
			ctx,
			name,
			dockertypes.ImageRemoveOptions{PruneChildren: true},
		)
		if e != nil && err == nil {
			err = e
		}
	}
	return err
}

// PushImage pushes an image to a registry, optionally using credentials from any set DOCKER_CONFIG
func PushImage(t *testing.T, dockerCli dockercli.CommonAPIClient, refStr string) {
	ref, err := name.ParseReference(refStr, name.WeakValidation)
	AssertNil(t, err)

	auth, err := authn.DefaultKeychain.Resolve(ref.Context().Registry)
	AssertNil(t, err)
	authConfig, err := auth.Authorization()
	AssertNil(t, err)

	encodedJSON, err := json.Marshal(authConfig)
	AssertNil(t, err)

	rc, err := dockerCli.ImagePush(context.Background(), refStr, dockertypes.ImagePushOptions{RegistryAuth: base64.URLEncoding.EncodeToString(encodedJSON)})
	AssertNil(t, err)
	defer rc.Close()

	AssertNil(t, checkResponseError(rc))

	_, err = io.Copy(ioutil.Discard, rc)
	AssertNil(t, err)
}

func ImageID(t *testing.T, repoName string) string {
	t.Helper()
	inspect, _, err := DockerCli(t).ImageInspectWithRaw(context.Background(), repoName)
	AssertNil(t, err)
	return inspect.ID
}

func CreateSingleFileTarReader(path, txt string) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		// Use the regular tar.Writer, as this isn't a layer tar.
		tw := tar.NewWriter(pw)

		if err := tw.WriteHeader(&tar.Header{Name: path, Size: int64(len(txt)), Mode: 0644}); err != nil {
			pw.CloseWithError(err)
		}

		if _, err := tw.Write([]byte(txt)); err != nil {
			pw.CloseWithError(err)
		}

		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
		}

		if err := pw.Close(); err != nil {
			pw.CloseWithError(err)
		}
	}()

	return pr
}

type layerWriter interface {
	WriteHeader(*tar.Header) error
	Write([]byte) (int, error)
	Close() error
}

func getLayerWriter(osType string, file *os.File) layerWriter {
	if osType == "windows" {
		return layer.NewWindowsWriter(file)
	}
	return tar.NewWriter(file)
}

func CreateSingleFileLayerTar(layerPath, txt, osType string) (string, error) {
	tarFile, err := ioutil.TempFile("", "create-single-file-layer-tar-path")
	if err != nil {
		return "", err
	}
	defer tarFile.Close()

	tw := getLayerWriter(osType, tarFile)

	if err := tw.WriteHeader(&tar.Header{Name: layerPath, Size: int64(len(txt)), Mode: 0644}); err != nil {
		return "", err
	}

	if _, err := tw.Write([]byte(txt)); err != nil {
		return "", err
	}

	if err := tw.Close(); err != nil {
		return "", err
	}

	return tarFile.Name(), nil
}

func FetchManifestLayers(t *testing.T, repoName string) []string {
	t.Helper()

	r, err := name.ParseReference(repoName, name.WeakValidation)
	AssertNil(t, err)

	auth, err := authn.DefaultKeychain.Resolve(r.Context().Registry)
	AssertNil(t, err)

	gImg, err := remote.Image(
		r,
		remote.WithTransport(http.DefaultTransport),
		remote.WithAuth(auth),
	)
	AssertNil(t, err)

	gLayers, err := gImg.Layers()
	AssertNil(t, err)

	var manifestLayers []string
	for _, gLayer := range gLayers {
		diffID, err := gLayer.DiffID()
		AssertNil(t, err)

		manifestLayers = append(manifestLayers, diffID.String())
	}

	return manifestLayers
}

func FetchManifestImageConfigFile(t *testing.T, repoName string) *v1.ConfigFile {
	t.Helper()

	r, err := name.ParseReference(repoName, name.WeakValidation)
	AssertNil(t, err)

	auth, err := authn.DefaultKeychain.Resolve(r.Context().Registry)
	AssertNil(t, err)

	gImg, err := remote.Image(r, remote.WithTransport(http.DefaultTransport), remote.WithAuth(auth))
	AssertNil(t, err)

	configFile, err := gImg.ConfigFile()
	AssertNil(t, err)

	return configFile
}

func FileDiffID(t *testing.T, path string) string {
	tarFile, err := os.Open(filepath.Clean(path))
	AssertNil(t, err)
	defer tarFile.Close()

	hasher := sha256.New()
	_, err = io.Copy(hasher, tarFile)
	AssertNil(t, err)

	diffID := "sha256:" + hex.EncodeToString(hasher.Sum(make([]byte, 0, hasher.Size())))

	return diffID
}

// RunnableBaseImage returns an image that can be used by a daemon of the same OS to create an container or run a command
func RunnableBaseImage(os string) string {
	if os == "windows" {
		// windows/amd64 image from manifest cached on github actions Windows 2019 workers: https://github.com/actions/virtual-environments/blob/master/images/win/Windows2019-Readme.md#docker-images
		return "mcr.microsoft.com/windows/nanoserver@sha256:08c883692e527b2bb4d7f6579e7707a30a2aaa66556b265b917177565fd76117"
	}
	return "busybox@sha256:915f390a8912e16d4beb8689720a17348f3f6d1a7b659697df850ab625ea29d5"
}

func StringElementAt(elements []string, offset int) string {
	if offset < 0 {
		return elements[len(elements)+offset]
	}
	return elements[offset]
}

func checkResponseError(r io.Reader) error {
	responseBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	responseBuf := bytes.NewBuffer(responseBytes)
	decoder := json.NewDecoder(responseBuf)

	for {
		var jsonMessage jsonmessage.JSONMessage
		err := decoder.Decode(&jsonMessage)

		if err != nil {
			return fmt.Errorf("parsing response: %w\n%s", err, responseBuf.String())
		}
		if jsonMessage.Error != nil {
			return errors.Wrap(jsonMessage.Error, "embedded daemon response")
		}
		if !decoder.More() {
			break
		}
	}

	return nil
}

func CreateSingleFileTar(path, txt string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: path, Size: int64(len(txt)), Mode: 0644}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(txt)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

func RandomLayer(t *testing.T, tmpDir string) (path string, sha string, contents []byte) {
	t.Helper()

	r, err := CreateSingleFileTar("/some-file", RandString(10))
	AssertNil(t, err)

	path = filepath.Join(tmpDir, RandString(10)+".tar")
	fh, err := os.Create(path)
	AssertNil(t, err)
	defer fh.Close()

	hasher := sha256.New()
	var contentsBuf bytes.Buffer
	mw := io.MultiWriter(hasher, fh, &contentsBuf)

	_, err = io.Copy(mw, r)
	AssertNil(t, err)

	sha = hex.EncodeToString(hasher.Sum(make([]byte, 0, hasher.Size())))

	return path, "sha256:" + sha, contentsBuf.Bytes()
}

func RemoteRunnableBaseImage(t *testing.T) v1.Image {
	testImageName := "busybox"
	var opts []remote.Option

	if runtime.GOOS == "windows" {
		testImageName = "mcr.microsoft.com/windows/nanoserver@sha256:8bd4389d56e69bebf6e4666251fba42f7cce3d5b768d28816884fb4370155fee" // mcr.microsoft.com/windows/nanoserver:1809

		windowsPlatform := v1.Platform{
			Architecture: "amd64",
			OS:           "windows",
			OSVersion:    "10.0.17763.3532",
		}
		opts = append(opts, remote.WithPlatform(windowsPlatform))
	}

	r, err := name.ParseReference(testImageName, name.WeakValidation)
	AssertNil(t, err)

	auth, err := authn.DefaultKeychain.Resolve(r.Context().Registry)
	AssertNil(t, err)

	opts = append(opts, remote.WithAuth(auth))

	testImage, err := remote.Image(r, opts...)
	AssertNil(t, err)

	return testImage
}

func AssertPathExists(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		t.Errorf("Expected %q to exist", path)
	} else if err != nil {
		t.Fatalf("Error stating %q: %v", path, err)
	}
}

func ReadIndexManifest(t *testing.T, path string) *v1.IndexManifest {
	indexPath := filepath.Join(path, "index.json")
	AssertPathExists(t, filepath.Join(path, "oci-layout"))
	AssertPathExists(t, indexPath)

	// check index file
	data, err := os.ReadFile(indexPath)
	AssertNil(t, err)

	index := &v1.IndexManifest{}
	err = json.Unmarshal(data, index)
	AssertNil(t, err)
	return index
}

func ReadManifest(t *testing.T, digest v1.Hash, path string) *v1.Manifest {
	manifestPath := filepath.Join(path, "blobs", digest.Algorithm, digest.Hex)
	AssertPathExists(t, manifestPath)

	data, err := os.ReadFile(manifestPath)
	AssertNil(t, err)

	manifest := &v1.Manifest{}
	err = json.Unmarshal(data, manifest)
	AssertNil(t, err)
	return manifest
}

func ReadConfigFile(t *testing.T, manifest *v1.Manifest, path string) *v1.ConfigFile {
	digest := manifest.Config.Digest
	configPath := filepath.Join(path, "blobs", digest.Algorithm, digest.Hex)
	AssertPathExists(t, configPath)

	data, err := os.ReadFile(configPath)
	AssertNil(t, err)

	configFile := &v1.ConfigFile{}
	err = json.Unmarshal(data, configFile)
	AssertNil(t, err)

	return configFile
}

func ReadManifestAndConfigFile(t *testing.T, path string) (*v1.Manifest, *v1.ConfigFile) {
	index := ReadIndexManifest(t, path)
	AssertEq(t, len(index.Manifests), 1)

	// TODO add platform to select the Manifest
	manifest := ReadManifest(t, index.Manifests[0].Digest, path)
	configFile := ReadConfigFile(t, manifest, path)
	return manifest, configFile
}
