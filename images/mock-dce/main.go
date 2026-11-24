// mock-dce：没有 DCE 时用它把链路跑起来（仅用于本地/演示，绝不用于生产）。
// 提供：JWKS、签发测试 JWT、current-user、clustercert、命名空间与 Pod 的假数据。
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	key      *rsa.PrivateKey
	issuer   = env("JWT_ISSUER", "http://mock-dce.local/realms/mock")
	audience = env("JWT_AUDIENCE", "mock-audience")
	// 交给 Actor 的 kubeconfig（base64 后的 YAML）；不填则返回空，kubectl 会失败但不影响对话链路
	kubeconfigB64 = os.Getenv("MOCK_KUBECONFIG_B64")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwks() map[string]any {
	n := b64(key.N.Bytes())
	e := b64(big.NewInt(int64(key.E)).Bytes())
	return map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "mock-key", "n": n, "e": e,
	}}}
}

func mint(user string) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "mock-key"})
	now := time.Now().Unix()
	c, _ := json.Marshal(map[string]any{
		"iss": issuer, "aud": audience, "sub": "mock-" + user,
		"preferred_username": user, "iat": now, "exp": now + 3600,
	})
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, 5 /*crypto.SHA256*/, sum[:])
	if err != nil {
		log.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	var err error
	key, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	addr := env("LISTEN", ":8090")

	http.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, jwks())
	})
	// 仅 mock：直接签发 token，方便脚本跑 verify
	http.HandleFunc("/dev/token", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		if user == "" {
			user = "alice"
		}
		writeJSON(w, 200, map[string]string{"token": mint(user), "username": user})
	})
	http.HandleFunc("/apis/ghippo.io/v1alpha1/current-user", func(w http.ResponseWriter, r *http.Request) {
		user := "alice"
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			user = "alice" // mock 不解析 token，verify 会用 /dev/token 里指定的用户
		}
		writeJSON(w, 200, map[string]any{"username": user, "uid": "mock-" + user})
	})
	http.HandleFunc("/apis/kpanda.io/v1alpha1/clusters/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clustercert"):
			writeJSON(w, 200, map[string]string{"kubeConfigString": kubeconfigB64})
		case strings.Contains(r.URL.Path, "/pods"):
			writeJSON(w, 200, map[string]any{"items": []any{map[string]any{
				"metadata": map[string]any{"name": "mock-pod", "namespace": "alice"},
				"status":   map[string]any{"phase": "Running"},
			}}})
		default:
			writeJSON(w, 200, map[string]any{"items": []any{}})
		}
	})
	http.HandleFunc("/apis/kpanda.io/v1alpha1/asl/namespaces", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alice"}},
		}})
	})
	log.Printf("mock-dce listening on %s (issuer=%s)", addr, issuer)
	log.Fatal(http.ListenAndServe(addr, nil))
}
