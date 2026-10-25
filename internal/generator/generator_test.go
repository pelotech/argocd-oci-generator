package generator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const prefix = "example/apps"

// fakeRegistry is go-containerregistry's in-memory registry behind TLS and Basic auth, test-only; it records
// every request path and how many came without credentials.
type fakeRegistry struct {
	server *httptest.Server
	auth   *authn.Basic
	mu     sync.Mutex
	paths  []string
	unauth int
	fail   int // when set, every request answers this status
}

func newRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{auth: &authn.Basic{Username: "tester", Password: "secret"}}
	inner := registry.New()
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		fail := f.fail
		f.mu.Unlock()
		if fail != 0 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(fail)
			return
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.auth.Username+":"+f.auth.Password))
		if r.Header.Get("Authorization") != want {
			f.mu.Lock()
			f.unauth++
			f.mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.server.URL, "https://") }

func (f *fakeRegistry) count(part string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if part == "/v2/" && p == part || part != "/v2/" && strings.Contains(p, part) {
			n++
		}
	}
	return n
}

func targz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "./" + n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// push publishes a single-layer artifact with annotations, the way oras does.
func (f *fakeRegistry) push(t *testing.T, repo, tag string, files map[string]string, annotations map[string]string) {
	t.Helper()
	img := artifact(t, files, annotations)
	ref, err := name.NewTag(fmt.Sprintf("%s/%s/%s:%s", f.host(), prefix, repo, tag))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithTransport(f.server.Client().Transport), remote.WithAuth(f.auth)); err != nil {
		t.Fatal(err)
	}
}

// artifact builds a single-layer artifact image, the way oras does.
func artifact(t *testing.T, files map[string]string, annotations map[string]string) v1.Image {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	img, err := mutate.AppendLayers(img, static.NewLayer(targz(files), types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	if annotations != nil {
		img = mutate.Annotations(img, annotations).(v1.Image)
	}
	return img
}

// pushIndex publishes an image index with one child per platform.
func (f *fakeRegistry) pushIndex(t *testing.T, repo, tag string, children map[string]v1.Image, annotations map[string]string) string {
	t.Helper()
	var index v1.ImageIndex = mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for platform, img := range children {
		os, arch, _ := strings.Cut(platform, "/")
		index = mutate.AppendManifests(index, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}}})
	}
	index = mutate.Annotations(index, annotations).(v1.ImageIndex)
	ref, err := name.NewTag(fmt.Sprintf("%s/%s/%s:%s", f.host(), prefix, repo, tag))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, index, remote.WithTransport(f.server.Client().Transport), remote.WithAuth(f.auth)); err != nil {
		t.Fatal(err)
	}
	digest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

func newGenerator(t *testing.T, f *fakeRegistry, minInterval time.Duration) (*Generator, *time.Time) {
	t.Helper()
	static := auth.StaticCredential(f.host(), auth.Credential{Username: f.auth.Username, Password: f.auth.Password})
	g, err := New(f.host()+"/"+prefix, static, minInterval)
	if err != nil {
		t.Fatal(err)
	}
	g.client = newClient(f.server.Client().Transport, static, quickRetry)
	now := time.Now()
	g.now = func() time.Time { return now }
	f.mu.Lock()
	f.paths, f.unauth = nil, 0
	f.mu.Unlock()
	return g, &now
}

var quickRetry = &retry.GenericPolicy{Retryable: retry.DefaultPredicate, Backoff: retry.DefaultBackoff, MinWait: time.Millisecond, MaxWait: 5 * time.Millisecond, MaxRetry: 2}

func daysAgo(n float64) string {
	return time.Now().UTC().Add(-time.Duration(n * float64(24*time.Hour))).Format(time.RFC3339)
}

func tags(entries []Entry) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Tag)
	}
	return out
}

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func payments(t *testing.T, f *fakeRegistry) {
	t.Helper()
	config := map[string]string{"config.json": `{"name": "payments"}`}
	for tag, created := range map[string]string{"3.0.10": daysAgo(5), "3.0.6": daysAgo(40), "2.9.0": daysAgo(400)} {
		f.push(t, "services/payments", tag, config, map[string]string{"org.opencontainers.image.version": tag, createdAnnotation: created})
	}
	f.push(t, "services/payments", "latest", config, nil)
	f.push(t, "services/payments", "3.0", config, nil)
	f.push(t, "services/billing", "latest", map[string]string{"config.json": "{}"}, nil)
}

func TestConstraints(t *testing.T) {
	f := newRegistry(t)
	for _, tag := range []string{"3.0.6", "3.0", "latest", "3.0.10", "v1.2.3", "3.1.0-rc.1", "3.1.0", "4.0.0", "2.9.9"} {
		f.push(t, "lib", tag, map[string]string{"a": "b"}, nil)
	}
	g, _ := newGenerator(t, f, 0)
	ten := 10
	for text, want := range map[string][]string{
		"*":              {"4.0.0", "3.1.0", "3.0.10", "3.0.6", "2.9.9", "v1.2.3"},
		"3.x":            {"3.1.0", "3.0.10", "3.0.6"},
		"3.0.x":          {"3.0.10", "3.0.6"},
		"^3.0.6":         {"3.1.0", "3.0.10", "3.0.6"},
		"~3.0.6":         {"3.0.10", "3.0.6"},
		">=3.0.6 <4":     {"3.1.0", "3.0.10", "3.0.6"},
		">=3.0.6, <3.1":  {"3.0.10", "3.0.6"},
		"!=3.0.10 3.0.x": {"3.0.6"},
	} {
		got, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"lib"}, Tag: text, Keep: &Keep{Count: &ten}})
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if !equal(tags(got), want) {
			t.Errorf("%s: got %v want %v", text, tags(got), want)
		}
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"lib"}, Tag: "nope", Keep: &Keep{Count: &ten}}); err == nil {
		t.Error("a word that is neither a tag nor a constraint should fail")
	}
}

func TestExactTagSkipsTheListAndFallsBack(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, _ := newGenerator(t, f, 0)
	in := Inputs{Repositories: []string{"services/payments", "services/billing"}, File: "config.json"}
	got, err := g.Parameters(context.Background(), Inputs{Repositories: in.Repositories, Tag: "latest", File: in.File})
	if err != nil {
		t.Fatal(err)
	}
	if !equal(tags(got), []string{"latest", "latest"}) || f.count("/tags/list") != 0 {
		t.Errorf("exact tags: %v, lists %d", tags(got), f.count("/tags/list"))
	}
	if *got[0].Content != `{"name": "payments"}` || got[0].Name != "services/payments" || got[0].Repository != prefix+"/services/payments" {
		t.Errorf("entry: %+v", got[0])
	}
	got, err = g.Parameters(context.Background(), Inputs{Repositories: in.Repositories, Tag: "3.0.7"})
	if err != nil || len(got) != 0 || f.count("/tags/list") != 2 {
		t.Errorf("missing exact tag: %v %v lists %d", got, err, f.count("/tags/list"))
	}
	got, err = g.Parameters(context.Background(), Inputs{Repositories: in.Repositories, Tag: "3.x"})
	if err != nil || !equal(tags(got), []string{"3.0.10"}) || got[0].Annotations["org.opencontainers.image.version"] != "3.0.10" {
		t.Errorf("constraint: %v %v", tags(got), err)
	}
}

func TestKeepCountAndDays(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, _ := newGenerator(t, f, 0)
	two, one, ninety, oneDay := 2, 1, 90.0, 1.0
	repos := []string{"services/payments"}
	got, err := g.Parameters(context.Background(), Inputs{Repositories: repos, Tag: "3.x", Keep: &Keep{Count: &two}})
	if err != nil || !equal(tags(got), []string{"3.0.10", "3.0.6"}) {
		t.Errorf("count: %v %v", tags(got), err)
	}
	manifests := f.count("/manifests/")
	got, err = g.Parameters(context.Background(), Inputs{Repositories: repos, Keep: &Keep{Days: &ninety}})
	if err != nil || !equal(tags(got), []string{"3.0.10", "3.0.6"}) {
		t.Errorf("days: %v %v", tags(got), err)
	}
	if f.count("/manifests/")-manifests != 3 {
		t.Errorf("days should read the kept two and the one that stopped the walk, read %d", f.count("/manifests/")-manifests)
	}
	got, err = g.Parameters(context.Background(), Inputs{Repositories: repos, Keep: &Keep{Days: &ninety, Count: &one}})
	if err != nil || !equal(tags(got), []string{"3.0.10"}) {
		t.Errorf("days and count: %v %v", tags(got), err)
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: repos, Tag: "latest", Keep: &Keep{Days: &oneDay}}); err == nil {
		t.Error("a floating tag has no created annotation and no version; days must fail")
	}
}

func TestOneAuthorizationPerPollAndBlobOncePerDigest(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, now := newGenerator(t, f, 0)
	in := Inputs{Repositories: []string{"services/payments", "services/billing"}, Tag: "*", File: "config.json"}
	if _, err := g.Parameters(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	blobs := f.count("/blobs/")
	*now = now.Add(time.Second)
	if _, err := g.Parameters(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if f.unauth != 1 {
		t.Errorf("one challenge, then the cached authorization for every request, got %d challenges", f.unauth)
	}
	if f.count("/blobs/") != blobs {
		t.Errorf("a blob is fetched once per digest, got %d then %d", blobs, f.count("/blobs/"))
	}
}

func TestImageIndexReadsThePlatformChild(t *testing.T) {
	f := newRegistry(t)
	digest := f.pushIndex(t, "tool", "1.0.0", map[string]v1.Image{
		"linux/amd64": artifact(t, map[string]string{"config.json": "amd64"}, nil),
		"linux/arm64": artifact(t, map[string]string{"config.json": "arm64"}, nil),
	}, map[string]string{"org.opencontainers.image.version": "1.0.0"})
	g, _ := newGenerator(t, f, 0)
	got, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"tool"}, File: "config.json"})
	if err != nil || len(got) != 1 || *got[0].Content != "amd64" || got[0].Digest != digest || got[0].Annotations["org.opencontainers.image.version"] != "1.0.0" {
		t.Errorf("default platform: %+v %v", got, err)
	}
	got, err = g.Parameters(context.Background(), Inputs{Repositories: []string{"tool"}, File: "config.json", Platform: "linux/arm64"})
	if err != nil || *got[0].Content != "arm64" {
		t.Errorf("linux/arm64: %+v %v", got, err)
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"tool"}, File: "config.json", Platform: "windows/amd64"}); err == nil || !strings.Contains(err.Error(), "linux/amd64, linux/arm64") {
		t.Errorf("a missing platform names the available ones: %v", err)
	}
	got, err = g.Parameters(context.Background(), Inputs{Repositories: []string{"tool"}})
	if err != nil || got[0].Content != nil || f.count("/blobs/") != 2 {
		t.Errorf("without file no child is read: %+v %v blobs %d", got, err, f.count("/blobs/"))
	}
}

func TestManyRegistriesThroughADockerConfig(t *testing.T) {
	a, b := newRegistry(t), newRegistry(t)
	b.auth = &authn.Basic{Username: "other", Password: "pass"}
	a.push(t, "services/payments", "1.0.0", map[string]string{"config.json": "from a"}, nil)
	b.push(t, "services/payments", "2.0.0", map[string]string{"config.json": "from b"}, nil)
	config := fmt.Sprintf(`{"auths": {%q: {"auth": %q}, %q: {"auth": %q}}}`,
		a.host(), base64.StdEncoding.EncodeToString([]byte("tester:secret")),
		b.host(), base64.StdEncoding.EncodeToString([]byte("other:pass")))
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.NewStore(path, credentials.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := New("", credentials.Credential(store), 0)
	if err != nil {
		t.Fatal(err)
	}
	g.client = newClient(a.server.Client().Transport, credentials.Credential(store), quickRetry)
	got, err := g.Parameters(context.Background(), Inputs{File: "config.json", Repositories: []string{
		a.host() + "/" + prefix + "/services/payments", b.host() + "/" + prefix + "/services/payments",
	}})
	if err != nil || len(got) != 2 {
		t.Fatalf("two registries: %+v %v", got, err)
	}
	if got[0].Registry != a.host() || *got[0].Content != "from a" || got[1].Registry != b.host() || *got[1].Content != "from b" || got[1].Tag != "2.0.0" {
		t.Errorf("entries: %+v", got)
	}
	if got[0].Repository != prefix+"/services/payments" {
		t.Errorf("repository without the host: %s", got[0].Repository)
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"services/payments"}}); err == nil {
		t.Error("a repository without a host needs REGISTRY")
	}
}

func TestTwoFilesOfOneArtifactAndTheReadCap(t *testing.T) {
	f := newRegistry(t)
	big := strings.Repeat("x", MaxFileSize+1)
	f.push(t, "bundle", "1.0.0", map[string]string{"config.json": "config", "values.yaml": "values", ".env": "dot", "big.bin": big}, nil)
	g, _ := newGenerator(t, f, 0)
	for file, want := range map[string]string{"config.json": "config", "values.yaml": "values", ".env": "dot"} {
		got, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"bundle"}, File: file})
		if err != nil || *got[0].Content != want {
			t.Errorf("%s: %v %v", file, got, err)
		}
	}
	if f.count("/blobs/") != 3 {
		t.Errorf("one blob read per file, got %d", f.count("/blobs/"))
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"bundle"}, File: "big.bin"}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("a file over MaxFileSize fails the poll: %v", err)
	}
	if _, err := g.Parameters(context.Background(), Inputs{Repositories: []string{"bundle"}, File: "missing"}); err == nil || !strings.Contains(err.Error(), "not in the artifact") {
		t.Errorf("a missing file fails the poll: %v", err)
	}
}

func TestConcurrentPolls(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, _ := newGenerator(t, f, 0)
	inputs := []Inputs{
		{Repositories: []string{"services/payments"}, Tag: "3.x", File: "config.json"},
		{Repositories: []string{"services/billing"}, Tag: "latest", File: "config.json"},
		{Repositories: []string{"services/payments"}, Tag: "3.x", File: "config.json"},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(inputs)*4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := g.Parameters(context.Background(), inputs[i%len(inputs)])
			if err == nil && len(got) != 1 {
				err = fmt.Errorf("%d entries", len(got))
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("poll %d: %v", i, err)
		}
	}
}

func TestMinIntervalServesTheLastAnswer(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, now := newGenerator(t, f, 5*time.Minute)
	in := Inputs{Repositories: []string{"services/payments"}}
	first, err := g.Parameters(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	requests := len(f.paths)
	*now = now.Add(299 * time.Second)
	again, _ := g.Parameters(context.Background(), in)
	if !equal(tags(again), tags(first)) || len(f.paths) != requests {
		t.Error("no request within the interval")
	}
	*now = now.Add(2 * time.Second)
	_, _ = g.Parameters(context.Background(), in)
	if len(f.paths) == requests {
		t.Error("a poll after the interval")
	}
}

func TestThrottledRegistryHoldsTheLastAnswer(t *testing.T) {
	f := newRegistry(t)
	payments(t, f)
	g, now := newGenerator(t, f, 0)
	in := Inputs{Repositories: []string{"services/payments"}}
	first, err := g.Parameters(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	f.fail = http.StatusTooManyRequests
	*now = now.Add(time.Second)
	held, err := g.Parameters(context.Background(), in)
	if err != nil || !equal(tags(held), tags(first)) {
		t.Errorf("the last answer on 429: %v %v", tags(held), err)
	}
	requests := len(f.paths)
	*now = now.Add(30 * time.Second)
	_, _ = g.Parameters(context.Background(), in)
	if len(f.paths) != requests {
		t.Error("no request while holding")
	}
	f.fail = 0
	*now = now.Add(time.Minute)
	if _, err := g.Parameters(context.Background(), in); err != nil || len(f.paths) == requests {
		t.Errorf("a poll after the hold: %v", err)
	}
	g2, _ := newGenerator(t, f, 0)
	f.fail = http.StatusServiceUnavailable
	if _, err := g2.Parameters(context.Background(), in); err == nil {
		t.Error("without a last answer the error surfaces")
	}
}
