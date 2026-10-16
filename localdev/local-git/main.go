// git-server serves the bare repositories under one directory over git's smart HTTP, to
// whoever presents the one credential: local development's git host (git_host provider plain, GIT_HOST_PROVIDER=local).
package main

import (
	"crypto/subtle"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	env := map[string]string{}
	for _, key := range []string{"GIT_SERVER_ADDRESS", "GIT_SERVER_ROOT", "GIT_SERVER_USERNAME", "GIT_SERVER_PASSWORD"} {
		if env[key] = os.Getenv(key); env[key] == "" {
			log.Fatalf("git-server: %s is required", key)
		}
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		log.Fatalf("git-server: ask git for its exec path: %v", err)
	}
	// git http-backend takes a push only from a user the server says it authenticated.
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + env["GIT_SERVER_ROOT"], "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + env["GIT_SERVER_USERNAME"]},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		username, password, _ := r.BasicAuth()
		if subtle.ConstantTimeCompare([]byte(username), []byte(env["GIT_SERVER_USERNAME"]))&subtle.ConstantTimeCompare([]byte(password), []byte(env["GIT_SERVER_PASSWORD"])) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServe(env["GIT_SERVER_ADDRESS"], mux))
}
