// Command argocd-oci-generator serves the Argo CD ApplicationSet plugin generator over one OCI registry.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/pelotech/argocd-oci-generator/internal/generator"
	"github.com/pelotech/argocd-oci-generator/internal/server"
)

// credential looks a host up in the docker config file of CREDENTIALS_FILE, with REGISTRY_USER and
// REGISTRY_PASSWORD for the default registry when set.
func credential() (auth.CredentialFunc, error) {
	host, _, _ := strings.Cut(os.Getenv("REGISTRY"), "/")
	lookup := auth.CredentialFunc(func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil })
	if path := os.Getenv("CREDENTIALS_FILE"); path != "" {
		store, err := credentials.NewStore(path, credentials.StoreOptions{})
		if err != nil {
			return nil, err
		}
		lookup = credentials.Credential(store)
	}
	if user := os.Getenv("REGISTRY_USER"); user != "" {
		static := auth.Credential{Username: user, Password: os.Getenv("REGISTRY_PASSWORD")}
		next := lookup
		lookup = func(ctx context.Context, h string) (auth.Credential, error) {
			if h == host {
				return static, nil
			}
			return next(ctx, h)
		}
	}
	return lookup, nil
}

func main() {
	cred, err := credential()
	if err != nil {
		log.Fatalf("CREDENTIALS_FILE: %v", err)
	}
	interval := 300.0
	if s := os.Getenv("MIN_INTERVAL"); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			log.Fatalf("MIN_INTERVAL: %v", err)
		}
		interval = v
	}
	g, err := generator.New(os.Getenv("REGISTRY"), cred, time.Duration(interval*float64(time.Second)))
	if err != nil {
		log.Fatalf("REGISTRY: %v", err)
	}
	token := os.Getenv("PLUGIN_TOKEN")
	if token == "" {
		log.Fatal("PLUGIN_TOKEN is empty; the generator refuses every call without one")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: server.Handler(g, token), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	log.Printf("listening on :%s for %s", port, os.Getenv("REGISTRY"))
	log.Fatal(srv.ListenAndServe())
}
