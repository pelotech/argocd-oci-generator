// Package generator polls OCI registries for the artifacts an ApplicationSet names: one parameter set per kept tag.
//
// For each repository the set names, a full reference or a path below the default registry prefix, the tags kept by the set's version constraint
// and keep rule become one parameter set each: name, repository, tag, digest, the manifest annotations and,
// when the set names a file, that file read out of the artifact's single tar.gz layer. Only the OCI
// distribution API, through oras-go: tag lists, manifests and blobs.
//
// Registry load: oras-go caches one authorization per registry and scope and retries 429 and 5xx with
// Retry-After; on top, blobs are fetched once per layer digest, an answer is reused for MIN_INTERVAL, and a
// poll the registry still refuses holds the last answer for a minute.
package generator

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const createdAnnotation = "org.opencontainers.image.created"

var tagSyntax = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// Inputs are the set's input.parameters.
type Inputs struct {
	Repositories []string `json:"repositories"`
	Tag          string   `json:"tag"`
	Keep         *Keep    `json:"keep"`
	File         string   `json:"file"`
	// Platform selects the child of an image index the file is read from, os/arch[/variant]; default linux/amd64.
	Platform string `json:"platform"`
}

// Keep selects how many kept tags a repository yields: the newest Count, those at most Days old, or both.
type Keep struct {
	Count *int     `json:"count"`
	Days  *float64 `json:"days"`
}

// Entry is one parameter set.
type Entry struct {
	Name        string            `json:"name"`
	Registry    string            `json:"registry"`
	Repository  string            `json:"repository"`
	Tag         string            `json:"tag"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
	Content     *string           `json:"content,omitempty"`
}

type answer struct {
	at      time.Time
	entries []Entry
}

// MaxFileSize bounds a file read out of a layer; a larger entry fails the poll instead of exhausting memory.
const MaxFileSize = 1 << 20

// Generator polls registries for the artifacts a set names.
type Generator struct {
	host        string // the default registry, for repositories named without a host
	prefix      string
	client      *auth.Client
	minInterval time.Duration
	hold        time.Duration
	now         func() time.Time

	mu        sync.Mutex             // guards the maps and holdUntil, never held across a registry call
	blobs     map[string]string      // layer digest and file -> content
	answers   map[string]answer      // inputs -> last answer
	polls     map[string]*sync.Mutex // inputs -> the poll in flight, so one set never polls twice at once
	holdUntil time.Time
}

// New takes the default registry as host/prefix, which repositories named without a host are relative to, and
// the credential lookup by host. A credential is sent as the registry's challenge asks, Basic or a Bearer token
// from its realm.
func New(defaultRegistry string, credential auth.CredentialFunc, minInterval time.Duration) (*Generator, error) {
	host, prefix, _ := strings.Cut(defaultRegistry, "/")
	if host == "" && prefix != "" {
		return nil, errors.New("REGISTRY is host or host/prefix")
	}
	return &Generator{
		host: host, prefix: prefix, client: newClient(transportWithTimeouts(), credential, retry.DefaultPolicy),
		minInterval: minInterval, hold: time.Minute, now: time.Now,
		blobs: map[string]string{}, answers: map[string]answer{}, polls: map[string]*sync.Mutex{},
	}, nil
}

// transportWithTimeouts is the default transport with bounds a blackholed registry cannot exceed.
func transportWithTimeouts() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

// newClient authenticates and retries: oras-go caches the token per registry and scope and backs off on 429
// and 5xx, honouring Retry-After.
func newClient(base http.RoundTripper, credential auth.CredentialFunc, policy retry.Policy) *auth.Client {
	return &auth.Client{
		Client:     &http.Client{Transport: &retry.Transport{Base: base, Policy: func() retry.Policy { return policy }}},
		Cache:      auth.NewCache(),
		Credential: credential,
	}
}

// hasHost is true for a reference whose first segment is a registry host, the way container references are read.
func hasHost(reference string) bool {
	first, _, _ := strings.Cut(reference, "/")
	return strings.ContainsAny(first, ".:") || first == "localhost"
}

// Parameters answers a poll: the last answer while it is fresh or the registry asked for a pause, else a new
// poll. A 429 or 5xx the retries did not clear keeps the last answer and holds before the next poll. Polls of
// different inputs run concurrently; the same inputs wait for the poll in flight and then reuse its answer.
func (g *Generator) Parameters(ctx context.Context, in Inputs) ([]Entry, error) {
	raw, _ := json.Marshal(in)
	key := string(raw)
	g.mu.Lock()
	poll, ok := g.polls[key]
	if !ok {
		poll = &sync.Mutex{}
		g.polls[key] = poll
	}
	g.mu.Unlock()
	poll.Lock()
	defer poll.Unlock()

	g.mu.Lock()
	now := g.now()
	last, answered := g.answers[key]
	fresh := answered && (now.Sub(last.at) < g.minInterval || now.Before(g.holdUntil))
	g.mu.Unlock()
	if fresh {
		return last.entries, nil
	}
	entries, err := g.fetch(ctx, in)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if throttled(err) {
			g.holdUntil = now.Add(g.hold)
			log.Printf("registry asked to wait: %v; holding %s", err, g.hold)
			if answered {
				return last.entries, nil
			}
		}
		return nil, err
	}
	g.answers[key] = answer{at: now, entries: entries}
	return entries, nil
}

func throttled(err error) bool {
	var er *errcode.ErrorResponse
	return errors.As(err, &er) && (er.StatusCode == http.StatusTooManyRequests || er.StatusCode >= 500)
}

// kept is one tag of a repository: its manifest, or its index with the child manifest resolved on demand.
type kept struct {
	tag         string
	digest      string
	annotations map[string]string
	manifest    *ocispec.Manifest
	index       *ocispec.Index
}

// repository is the set's name as a full reference, or below the default registry and prefix.
func (g *Generator) repository(name string) (*remote.Repository, error) {
	reference := name
	if !hasHost(name) {
		if g.host == "" {
			return nil, fmt.Errorf("repository %q names no registry and REGISTRY is not set", name)
		}
		reference = g.host + "/" + name
		if g.prefix != "" {
			reference = g.host + "/" + g.prefix + "/" + name
		}
	}
	repo, err := remote.NewRepository(reference)
	if err != nil {
		return nil, err
	}
	repo.Client = g.client
	return repo, nil
}

func (g *Generator) fetch(ctx context.Context, in Inputs) ([]Entry, error) {
	repos := make([]*remote.Repository, 0, len(in.Repositories))
	for _, n := range in.Repositories {
		repo, err := g.repository(n)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
		// One token for every repository of the poll.
		ctx = auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPull)
	}
	tag := in.Tag
	if tag == "" {
		tag = "*"
	}
	var keep Keep
	if in.Keep != nil {
		keep = *in.Keep
	}
	entries := []Entry{}
	for i, repo := range repos {
		kept, err := g.selectTags(ctx, repo, tag, keep)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo.Reference.Repository, err)
		}
		for _, k := range kept {
			entry := Entry{Name: in.Repositories[i], Registry: repo.Reference.Registry, Repository: repo.Reference.Repository, Tag: k.tag, Digest: k.digest, Annotations: k.annotations}
			if entry.Annotations == nil {
				entry.Annotations = map[string]string{}
			}
			if in.File != "" {
				content, err := g.content(ctx, repo, k, in.File, in.Platform)
				if err != nil {
					return nil, fmt.Errorf("%s:%s: %w", repo.Reference.Repository, k.tag, err)
				}
				entry.Content = &content
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// exactCandidate is a tag that may exist as such: no constraint operator and no wildcard component.
func exactCandidate(tag string) bool {
	if strings.ContainsAny(tag, "*^~<>=!, ") {
		return false
	}
	for _, part := range strings.Split(tag, ".") {
		if part == "x" || part == "X" {
			return false
		}
	}
	return tagSyntax.MatchString(tag)
}

// selectTags keeps, for one repository, the exact tag when it exists and one is kept, else the newest tags that
// satisfy the constraint, at most keep.count, and not older than keep.days by their created annotation,
// stopping at the first older one.
func (g *Generator) selectTags(ctx context.Context, repo *remote.Repository, tag string, keep Keep) ([]kept, error) {
	count, days := keep.Count, keep.Days
	if count == nil && days == nil {
		one := 1
		count = &one
	}
	if count != nil && *count == 1 && days == nil && exactCandidate(tag) {
		k, err := g.get(ctx, repo, tag)
		if err == nil {
			return []kept{k}, nil
		}
		if !errors.Is(err, errdef.ErrNotFound) {
			return nil, err
		}
	}
	constraint, err := semver.NewConstraint(tag)
	if err != nil {
		return nil, fmt.Errorf("tag %q is neither an existing tag nor a version constraint: %w", tag, err)
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(page []string) error { tags = append(tags, page...); return nil }); err != nil {
		return nil, err
	}
	type candidate struct {
		tag     string
		version *semver.Version
	}
	var candidates []candidate
	for _, t := range tags {
		v, err := semver.StrictNewVersion(strings.TrimPrefix(t, "v"))
		if err == nil && constraint.Check(v) {
			candidates = append(candidates, candidate{t, v})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].version.GreaterThan(candidates[j].version) })
	var cutoff time.Time
	if days != nil {
		cutoff = g.now().Add(-time.Duration(*days * float64(24*time.Hour)))
	}
	var found []kept
	for _, c := range candidates {
		if count != nil && len(found) >= *count {
			break
		}
		k, err := g.get(ctx, repo, c.tag)
		if errors.Is(err, errdef.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if days != nil {
			created, ok := k.annotations[createdAnnotation]
			if !ok {
				return nil, fmt.Errorf("%s has no %s annotation, which keep.days needs", c.tag, createdAnnotation)
			}
			at, err := time.Parse(time.RFC3339, created)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", c.tag, createdAnnotation, err)
			}
			if at.Before(cutoff) {
				break
			}
		}
		found = append(found, k)
	}
	return found, nil
}

func (g *Generator) get(ctx context.Context, repo *remote.Repository, tag string) (kept, error) {
	desc, rc, err := repo.FetchReference(ctx, tag)
	if err != nil {
		return kept{}, err
	}
	defer rc.Close()
	if desc.MediaType == ocispec.MediaTypeImageIndex || desc.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var index ocispec.Index
		if err := json.NewDecoder(rc).Decode(&index); err != nil {
			return kept{}, err
		}
		return kept{tag: tag, digest: desc.Digest.String(), annotations: index.Annotations, index: &index}, nil
	}
	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return kept{}, err
	}
	return kept{tag: tag, digest: desc.Digest.String(), annotations: manifest.Annotations, manifest: &manifest}, nil
}

// child resolves the manifest of an index for a platform, os/arch[/variant].
func (g *Generator) child(ctx context.Context, repo *remote.Repository, index *ocispec.Index, platform string) (*ocispec.Manifest, error) {
	if platform == "" {
		platform = "linux/amd64"
	}
	var available []string
	for _, desc := range index.Manifests {
		if desc.Platform == nil {
			continue
		}
		name := desc.Platform.OS + "/" + desc.Platform.Architecture
		if desc.Platform.Variant != "" {
			name += "/" + desc.Platform.Variant
		}
		available = append(available, name)
		if name != platform && (desc.Platform.Variant == "" || name != platform+"/"+desc.Platform.Variant) {
			continue
		}
		rc, err := repo.Manifests().Fetch(ctx, desc)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var manifest ocispec.Manifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			return nil, err
		}
		return &manifest, nil
	}
	sort.Strings(available)
	return nil, fmt.Errorf("the index has no %s manifest; it has %s", platform, strings.Join(available, ", "))
}

// content reads the named file out of the artifact's single layer, once per layer digest; for an index, out of
// the child manifest of the platform.
func (g *Generator) content(ctx context.Context, repo *remote.Repository, k kept, file, platform string) (string, error) {
	manifest := k.manifest
	if manifest == nil {
		var err error
		if manifest, err = g.child(ctx, repo, k.index, platform); err != nil {
			return "", err
		}
	}
	if len(manifest.Layers) != 1 {
		return "", fmt.Errorf("the artifact has %d layers; one tar.gz layer is expected", len(manifest.Layers))
	}
	layer := manifest.Layers[0]
	key := layer.Digest.String() + "\x00" + file
	g.mu.Lock()
	cached, ok := g.blobs[key]
	g.mu.Unlock()
	if ok {
		return cached, nil
	}
	rc, err := repo.Blobs().Fetch(ctx, layer)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	found, err := fileFromLayer(rc, file)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.blobs[key] = found
	g.mu.Unlock()
	return found, nil
}

// fileFromLayer returns one file of a tar.gz stream, by its path without a leading ./.
func fileFromLayer(r io.Reader, file string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("%s is not in the artifact", file)
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == file {
			if h.Size > MaxFileSize {
				return "", fmt.Errorf("%s is %d bytes; at most %d are read", file, h.Size, MaxFileSize)
			}
			b, err := io.ReadAll(io.LimitReader(tr, MaxFileSize+1))
			if err == nil && len(b) > MaxFileSize {
				err = fmt.Errorf("%s is larger than its header says; at most %d bytes are read", file, MaxFileSize)
			}
			return string(b), err
		}
	}
}
