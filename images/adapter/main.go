// sandbox-adapter: kgateway 之后的“身份 → 会话”绑定层。
//
// 职责只有两件：
//  1. 自行校验 DCE JWT（不复用网关的判断，防止绕过网关直达本服务）；
//  2. 把 A2A JSON-RPC 里的 contextId 强制改写成由 JWT 身份推导出的会话 id，
//     丢弃客户端传来的值——这样用户无法靠改参数进入别人的会话。
//
// 上游是 kagent 的 sandbox A2A 入口，substrate 会按 contextId 拉起/恢复对应 actor。
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type verifier struct {
	issuer   string
	audience string
	keys     map[string]*rsa.PublicKey
}

func loadVerifier(path, issuer, audience string) (*verifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set jwkSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, err
	}
	v := &verifier{issuer: issuer, audience: audience, keys: map[string]*rsa.PublicKey{}}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		v.keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	}
	if len(v.keys) == 0 {
		return nil, errors.New("no usable RSA signing key in JWKS")
	}
	return v, nil
}

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
}

// verify 返回 (sub, preferred_username, error)
func (v *verifier) verify(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", errors.New("malformed token")
	}
	headRaw, err := b64url(parts[0])
	if err != nil {
		return "", "", err
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headRaw, &head); err != nil {
		return "", "", err
	}
	if head.Alg != "RS256" {
		return "", "", fmt.Errorf("unsupported alg %q", head.Alg)
	}
	sig, err := b64url(parts[2])
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))

	keys := []*rsa.PublicKey{}
	if k, ok := v.keys[head.Kid]; ok {
		keys = append(keys, k)
	} else {
		for _, k := range v.keys {
			keys = append(keys, k)
		}
	}
	ok := false
	for _, k := range keys {
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil {
			ok = true
			break
		}
	}
	if !ok {
		return "", "", errors.New("bad signature")
	}

	payRaw, err := b64url(parts[1])
	if err != nil {
		return "", "", err
	}
	var claims struct {
		Iss               string          `json:"iss"`
		Aud               json.RawMessage `json:"aud"`
		Exp               int64           `json:"exp"`
		Sub               string          `json:"sub"`
		PreferredUsername string          `json:"preferred_username"`
	}
	if err := json.Unmarshal(payRaw, &claims); err != nil {
		return "", "", err
	}
	if v.issuer != "" && claims.Iss != v.issuer {
		return "", "", fmt.Errorf("issuer mismatch: %q", claims.Iss)
	}
	if v.audience != "" && !audienceContains(claims.Aud, v.audience) {
		return "", "", fmt.Errorf("audience mismatch: %s", string(claims.Aud))
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return "", "", errors.New("token expired")
	}
	return claims.Sub, claims.PreferredUsername, nil
}

func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// sessionID 由身份确定性地推导，客户端无法伪造；同一用户跨请求、跨 Pod 重启都落到同一会话。
// sessionEpoch 用于"换一代会话"：某个会话卡死时把它 +1，所有人拿到新的会话 id，
// 不必等控制面把坏掉的 actor 清掉。
// SESSION_EPOCH 已废弃：仅保留读取，不再参与会话推导（见 sessionIDFor）。
var sessionEpoch = env("SESSION_EPOCH", "")

// conversationId 由客户端生成，只允许安全字符，长度受限。
var convIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

func sessionID(identity string) string { return sessionIDFor(identity, "") }

// sessionIDFor 把"用户 + 会话标识"确定性地映射成一个 actor 会话。
// conversationId 由客户端生成（用于"新会话"），但它只会落在该用户自己的命名空间里：
// 同一个 conversationId 对 alice 和 bob 得到不同会话，且无法反推别人的会话。
// 注意：这里不再掺 SESSION_EPOCH。会话只由「用户 + 会话 id」决定，
// 所以换会话 = 换一个 conversationId 即可，不需要让全平台的旧会话一起作废。
const sessionSalt = "conv1"

func sessionIDFor(identity, conversationID string) string {
	seed := strings.ToLower(strings.TrimSpace(identity)) + "#" + sessionSalt
	if c := strings.TrimSpace(conversationID); c != "" {
		seed += "#" + c
	}
	sum := sha256.Sum256([]byte(seed))
	return "u-" + hex.EncodeToString(sum[:])[:24]
}

func identityOf(sub, preferred string) string {
	if strings.TrimSpace(preferred) != "" {
		return preferred
	}
	return sub
}

// ---- kagent 1.0：会话映射 ----------------------------------------------
// 0.10 的适配器直接把 contextId 当成"由身份推导的会话 id"传给上游。
// 1.0 里 Session 由服务端创建，ContextId 也是服务端生成的 UUID，调用方只能引用。
// 所以这里维护 sid(本地推导) -> contextId(上游) 的映射，并落盘，避免适配器重启后
// 同一个会话被拆成两个沙箱（历史丢失）。
var (
	// 沙箱里没有用户 JWT 时，kubectl 用 actor 身份（session-<contextId>）来认领凭据。
	idMu    sync.Mutex
	idJWT   = map[string]string{} // identity -> 最近一次见到的 JWT
	ctxOwner = map[string]string{} // contextId -> identity
	sessionMapPath = env("SESSION_MAP_FILE", "/data/adapter-sessions.json")
	sessionMapMu   sync.Mutex
	sessionMap     = map[string]string{}
)

func loadSessionMap() {
	raw, err := os.ReadFile(sessionMapPath)
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		sessionMapMu.Lock()
		sessionMap = m
		sessionMapMu.Unlock()
		log.Printf("loaded %d session mappings from %s", len(m), sessionMapPath)
	}
}

func sessionMapGet(sid string) string {
	sessionMapMu.Lock()
	defer sessionMapMu.Unlock()
	return sessionMap[sid]
}

func sessionMapSet(sid, contextID string) {
	if sid == "" || contextID == "" {
		return
	}
	sessionMapMu.Lock()
	defer sessionMapMu.Unlock()
	if sessionMap[sid] == contextID {
		return
	}
	sessionMap[sid] = contextID
	raw, err := json.Marshal(sessionMap)
	if err != nil {
		return
	}
	tmp := sessionMapPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, sessionMapPath)
	}
	log.Printf("session mapping %s -> %s", sid, contextID)
}

// applyContext 按上游实际存在的会话设置/清除 contextId。
// ctx 为空表示"这条会话在上游还没有"：此时必须删掉 contextId，让上游新建 Session。
func applyContext(body []byte, ctx string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	params, ok := doc["params"].(map[string]any)
	if !ok {
		return nil, errors.New("missing params")
	}
	if ctx == "" {
		delete(params, "contextId")
	} else {
		params["contextId"] = ctx
	}
	if msg, ok := params["message"].(map[string]any); ok {
		if ctx == "" {
			delete(msg, "contextId")
		} else {
			msg["contextId"] = ctx
		}
		// A2A 1.0 用枚举 ROLE_USER；0.3 的 "user" 会被上游判为 unspecified。
		msg["role"] = "ROLE_USER"
	}
	// 0.3 的方法名到 1.0 的映射（kagent 1.0 只服务 A2A 1.0）。
	switch doc["method"] {
	case "message/send":
		doc["method"] = "SendMessage"
	case "message/stream":
		doc["method"] = "SendStreamingMessage"
	}
	return json.Marshal(doc)
}

// a2aContextID 从 JSON-RPC 响应/流事件里取上游会话 id（Task/状态事件都带 contextId）。
func a2aContextID(raw []byte) string {
	var doc struct {
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Result == nil {
		return ""
	}
	if v, ok := doc.Result["contextId"].(string); ok && v != "" {
		return v
	}
	for _, k := range []string{"task", "statusUpdate", "artifactUpdate"} {
		if inner, ok := doc.Result[k].(map[string]any); ok {
			if v, ok := inner["contextId"].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// a2aEvent 解析 A2A 1.0 的一帧：增量文本 / 会话 id / 是否完成。
func a2aEvent(raw []byte) (delta, ctxID string, done bool) {
	var doc struct {
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Result == nil {
		return "", "", false
	}
	r := doc.Result
	ctxID = a2aContextID(raw)
	if au, ok := r["artifactUpdate"].(map[string]any); ok {
		if art, ok := au["artifact"].(map[string]any); ok {
			delta = partsText(art["parts"])
		}
	}
	if t, ok := r["task"].(map[string]any); ok {
		if st, ok := t["status"].(map[string]any); ok {
			s, _ := st["state"].(string)
			done = s == "TASK_STATE_COMPLETED" || s == "TASK_STATE_FAILED" || s == "TASK_STATE_CANCELED"
		}
	}
	if su, ok := r["statusUpdate"].(map[string]any); ok {
		if st, ok := su["status"].(map[string]any); ok {
			s, _ := st["state"].(string)
			if s == "TASK_STATE_COMPLETED" || s == "TASK_STATE_FAILED" || s == "TASK_STATE_CANCELED" {
				done = true
			}
		}
	}
	return delta, ctxID, done
}

func partsText(v any) string {
	parts, ok := v.([]any)
	if !ok {
		return ""
	}
	out := ""
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				out += t
			}
		}
	}
	return out
}


// ---- /poc/k8s：把沙箱里的 kubectl 请求转成"用户身份"的 apiserver 调用 --------
// 沙箱里不存放用户的客户端证书；kubectl 指向 adapter，用 DCE JWT 鉴权，
// adapter 用该用户的 clustercert 调 apiserver，权限仍由 K8s RBAC 判定。
func kubeField(kubeconfig, key string) []byte {
	for _, line := range strings.Split(kubeconfig, "\n") {
		if i := strings.Index(line, key); i >= 0 {
			v := strings.TrimSpace(line[i+len(key):])
			if b, err := base64.StdEncoding.DecodeString(v); err == nil {
				return b
			}
			return []byte(v)
		}
	}
	return nil
}

func rememberIdentity(identity, jwt string) {
	if identity == "" || jwt == "" {
		return
	}
	idMu.Lock()
	idJWT[identity] = jwt
	idMu.Unlock()
}

func rememberCtxOwner(ctx, identity string) {
	if ctx == "" || identity == "" {
		return
	}
	idMu.Lock()
	ctxOwner[ctx] = identity
	idMu.Unlock()
}

// jwtFor 支持两种凭据：直接给用户 JWT，或给沙箱自己的 actor 名（session-<contextId>）。
func jwtFor(tok string) (string, bool) {
	if strings.HasPrefix(tok, "session-") {
		ctx := strings.TrimPrefix(tok, "session-")
		idMu.Lock()
		id := ctxOwner[ctx]
		jwt := idJWT[id]
		idMu.Unlock()
		if jwt == "" {
			return "", false
		}
		return jwt, true
	}
	return tok, true
}

func credBlock(r *http.Request) string {
	tok := bearerRaw(r)
	return "\n\n【本轮凭据（POC）】\n" +
		"DCE_TOKEN=" + tok + "\n" +
		"用法：kubectl 已配好（无需 token）；dce-cli/sre-cli 需要把上面的值写在命令前，例如：\n" +
		"  DCE_TOKEN=" + tok + " dce-cli GET /apis/kpanda.io/v1alpha1/asl/namespaces?page=1&pageSize=50\n" +
		"  SRE_TOKEN=" + tok + " sre-cli report"
}

// ---- /poc/sre: 沙箱里的 sre-cli 经 adapter 转 SRE 后端（绕开挂住的 agentgateway）----
func sreProxyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		tok := bearerRaw(r)
		rest := strings.TrimPrefix(path, "/poc/sre")
		if strings.HasPrefix(path, "/poc/sre/s/") {
			rem := strings.TrimPrefix(path, "/poc/sre/s/")
			i := strings.Index(rem, "/")
			if i <= 0 {
				http.Error(w, "bad actor path", http.StatusBadRequest)
				return
			}
			tok = rem[:i]
			rest = rem[i:]
		}
		if _, ok := jwtFor(tok); !ok {
			http.Error(w, "actor 还没有绑定用户（先发一条对话再试）", http.StatusUnauthorized)
			return
		}
		backend := env("SRE_BACKEND_URL", "http://sre-poc.watsons-agent-poc.svc.cluster.local:8080")
		btok := os.Getenv("SRE_BACKEND_TOKEN")
		if btok == "" {
			http.Error(w, "adapter 缺少 SRE_BACKEND_TOKEN", http.StatusServiceUnavailable)
			return
		}
		target := backend + "/v1/reports/latest"
		method := r.Method
		var body io.Reader
		switch {
		case rest == "/report":
			target, method = backend+"/v1/reports/latest", http.MethodGet
		case rest == "/inspect":
			target, method = backend+"/v1/inspections", http.MethodPost
			body = bytes.NewReader([]byte(`{"cluster":"orbstack","scope":"health"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), method, target, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header.Set("Authorization", "Bearer "+btok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 180 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, "sre backend: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func k8sProxyHandler(v *verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 身份来源两种：URL 路径 /poc/k8s/s/<token>/...（沙箱内 kubectl 用，
		// 因为 Authorization 头会被 egress 侧吃掉），或 Bearer 头（本机/调试用）。
		path := r.URL.Path
		tok := bearerRaw(r)
		rest := strings.TrimPrefix(path, "/poc/k8s")
		if strings.HasPrefix(path, "/poc/k8s/s/") {
			rem := strings.TrimPrefix(path, "/poc/k8s/s/")
			i := strings.Index(rem, "/")
			if i <= 0 {
				http.Error(w, "bad actor path", http.StatusBadRequest)
				return
			}
			tok = rem[:i]
			rest = rem[i:]
		}
		if rest == "" {
			rest = "/"
		}
		if tok == "" {
			http.Error(w, "unauthorized: missing actor identity", http.StatusUnauthorized)
			return
		}
		preview := tok
		if len(preview) > 24 {
			preview = preview[:24] + "..."
		}
		log.Printf("k8s-proxy %s %s via=%q", r.Method, rest, preview)
		if !strings.HasPrefix(tok, "session-") {
			if _, _, err := bearer(v, r); err != nil {
				http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
				return
			}
		}
		jwt, ok := jwtFor(tok)
		if !ok {
			http.Error(w, "actor 还没有绑定用户（先发一条对话再试）", http.StatusUnauthorized)
			return
		}
		kc, err := dceKubeconfig(jwt)
		if err != nil {
			http.Error(w, "kubeconfig: "+err.Error(), http.StatusBadGateway)
			return
		}
		certPEM, keyPEM := kubeField(kc, "client-certificate-data:"), kubeField(kc, "client-key-data:")
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			http.Error(w, "kubeconfig has no client certificate", http.StatusBadGateway)
			return
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			http.Error(w, "client cert: "+err.Error(), http.StatusBadGateway)
			return
		}
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{pair}}}
		defer tr.CloseIdleConnections()
		target := "https://kubernetes.default.svc.cluster.local" + rest
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header.Set("Accept", r.Header.Get("Accept"))
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := (&http.Client{Timeout: 120 * time.Second, Transport: tr}).Do(req)
		if err != nil {
			http.Error(w, "apiserver: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func main() {
	var (
		addr     = env("ADAPTER_ADDR", ":9099")
		jwksPath = env("JWKS_FILE", "/etc/adapter/jwks.json")
		issuer   = env("JWT_ISSUER", "")
		audience = env("JWT_AUDIENCE", "")
		upstream = env("UPSTREAM", "http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/deepseek-agent")
	)
	loadSessionMap()
	v, err := loadVerifier(jwksPath, issuer, audience)
	if err != nil {
		log.Fatalf("load jwks: %v", err)
	}
	// 必须大于沙箱里 shim 的 turn 超时（180s），否则适配器先放弃会把这个会话拖死
	client := &http.Client{Timeout: 240 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	// /whoami 用于联调：返回本请求被绑定到的身份与会话，不触发沙箱。
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		sub, pref, err := bearer(v, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		id := identityOf(sub, pref)
		writeJSON(w, 200, map[string]string{"identity": id, "sessionId": sessionID(id)})
	})
	mux.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		sub, pref, err := bearer(v, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		id := identityOf(sub, pref)
		rememberIdentity(id, bearerRaw(r))
		sid := sessionID(id)

		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		ctxID := sessionMapGet(sid)
		rememberCtxOwner(ctxID, id)
		rewritten, err := applyContext(body, ctxID)
		if err != nil {
			http.Error(w, "invalid json-rpc body: "+err.Error(), http.StatusBadRequest)
			return
		}

		resp, err := doUpstream(r.Context(), client, upstream, r, id, sid, rewritten)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("X-Sandbox-Session", sid)
		w.WriteHeader(resp.StatusCode)
		if ctxID == "" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if c := a2aContextID(raw); c != "" {
				sessionMapSet(sid, c)
			}
			_, _ = w.Write(raw)
			return
		}
		_, _ = io.Copy(w, resp.Body)
	})

	// 兼容老 POC 的 Chrome 插件（它调 /poc/whoami 和 /poc/chat）。
	// session 同样由 JWT 推导，插件传的 session_id 一律忽略。
	// 流式：把上游 A2A 的 SSE 增量转成前端好用的 {delta} / {done}
	// 浏览器扩展的内容脚本直接发起流式请求（不经 MV3 后台），需要 CORS。
	// 只允许 DCE 控制台的源；Authorization 头必须显式放行，否则预检会失败。
	mux.HandleFunc("/poc/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	allowedOrigin := func(o string) bool {
		return o == "http://10.31.223.20:31284" || strings.HasPrefix(o, "http://localhost") || strings.HasPrefix(o, "http://127.0.0.1")
	}
	withCORS := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" && allowedOrigin(o) {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/poc/chat/stream", withCORS(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		sub, pref, err := bearer(v, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		id := identityOf(sub, pref)
		var in struct {
			Message        string `json:"message"`
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		sid := sessionID(id)
		rememberIdentity(id, bearerRaw(r))
		if in.ConversationID != "" {
			if !convIDRe.MatchString(in.ConversationID) {
				http.Error(w, "invalid conversation_id", http.StatusBadRequest)
				return
			}
			sid = sessionIDFor(id, in.ConversationID)
		}
		parts := []map[string]any{{"kind": "text", "text": in.Message + credBlock(r)}}
		parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"sreToken": bearerRaw(r)}})
		if kc, kerr := dceKubeconfig(bearerRaw(r)); kerr == nil {
			parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"kubeconfig": kc}})
		}
		ctxID := sessionMapGet(sid)
		rememberCtxOwner(ctxID, id)
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "str-" + sid, "method": "SendStreamingMessage",
			"params": map[string]any{
				"message": map[string]any{"role": "ROLE_USER", "kind": "message",
					"messageId": "str-" + sid + "-" + strconv.FormatInt(time.Now().UnixNano(), 36), "parts": parts},
				"configuration": map[string]any{"acceptedOutputModes": []string{"text"}},
			},
		})
		if ctxID != "" {
			payload, _ = applyContext(payload, ctxID)
		}
		flusher, _ := w.(http.Flusher)
		var emit func(any)
		started := false
		startStream := func() {
			if started {
				return
			}
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Sandbox-Session", sid)
			w.WriteHeader(200)
			emit = func(obj any) {
				raw, _ := json.Marshal(obj)
				_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		// 1.0 里新建 Session 需要先把它拉起来（actor 创建），这段时间上游会回
		// KAGENT_SEND_NOT_ACCEPTED。首帧就是这个错时整轮重试，否则用户会看到
		// "input was not accepted"。已经开始向客户端发流之后就不再重试。
		acc := ""
		doneSent := false
		var lastErr any
		for attempt := 0; attempt < 90; attempt++ {
			if payload2, err := applyContext(payload, sessionMapGet(sid)); err == nil {
				payload = payload2
			}
			resp, err := doUpstream(r.Context(), client, upstream, r, id, sid, payload)
			if err != nil {
				if !started {
					writeJSON(w, 502, map[string]string{"error": "agent unavailable: " + err.Error()})
					return
				}
				break
			}
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 1<<20)
			first := true
			retry := false
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				frame := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				var ev struct {
					Error any `json:"error"`
				}
				if json.Unmarshal(frame, &ev) != nil {
					continue
				}
				if ev.Error != nil {
					if first && isSendNotAccepted(frame) {
						lastErr = ev.Error
						retry = true
						break
					}
					startStream()
					emit(map[string]any{"error": ev.Error})
					first = false
					continue
				}
				first = false
				startStream()
				delta, ctx, done := a2aEvent(frame)
				if sessionMapGet(sid) == "" && ctx != "" {
					sessionMapSet(sid, ctx)
					rememberCtxOwner(ctx, id)
				}
				if delta != "" {
					// 上游对同一段文本可能既发增量又发全量（append=false 表示替换），
					// 这里按"已发过的前缀"去重，避免回复重复一遍。
					switch {
					case delta == acc:
						// 全量重复，跳过
					case strings.HasPrefix(delta, acc) && len(delta) > len(acc):
						emit(map[string]string{"delta": delta[len(acc):]})
						acc = delta
					default:
						emit(map[string]string{"delta": delta})
						acc += delta
					}
				}
				if done && !doneSent {
					doneSent = true
					emit(map[string]any{"done": true, "session_id": sid})
				}
			}
			resp.Body.Close()
			if !retry {
				break
			}
			time.Sleep(time.Second)
		}
		if lastErr != nil {
			startStream()
			emit(map[string]any{"error": lastErr})
		}
		startStream()
		if !doneSent {
			emit(map[string]any{"done": true, "session_id": sid})
		}
	}))

	mux.HandleFunc("/poc/whoami", func(w http.ResponseWriter, r *http.Request) {
		sub, pref, err := bearer(v, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		id := identityOf(sub, pref)
		writeJSON(w, 200, map[string]string{"username": id, "session_id": sessionID(id)})
	})
	mux.HandleFunc("/poc/sre/", withCORS(sreProxyHandler()))
	mux.HandleFunc("/poc/k8s/", withCORS(k8sProxyHandler(v)))
	mux.HandleFunc("/poc/k8s", withCORS(k8sProxyHandler(v)))
	mux.HandleFunc("/poc/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		sub, pref, err := bearer(v, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		id := identityOf(sub, pref)
		sid := sessionID(id)

		var in struct {
			Message        string `json:"message"`
			SessionID      string `json:"session_id"`
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Message) == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if in.ConversationID != "" {
			if !convIDRe.MatchString(in.ConversationID) {
				http.Error(w, "invalid conversation_id", http.StatusBadRequest)
				return
			}
			sid = sessionIDFor(id, in.ConversationID)
		}
		if in.SessionID != "" && in.SessionID != sid {
			log.Printf("identity=%s ignored client session_id=%q (conv=%q)", id, in.SessionID, in.ConversationID)
		}

		// 给沙箱里的 DSH 一段环境说明与任务规则，避免误判能力、也避免用自检冒充 SRE 报告。
		const preamble = "【运行环境】Linux 沙箱，可执行 shell/Python、可联网；kubectl 已装，KUBECONFIG 是当前用户的只读集群凭据。" +
			"【可用的外部能力】1) SRE Agent：node /opt/dsh-a2a/sre-cli.js report（取最新巡检报告）/ sre-cli inspect（发起一次新巡检）；" +
			"2) DCE OpenAPI：node /opt/dsh-a2a/dce-cli.js GET /apis/...（以当前用户身份，权限由 DCE 判断）；" +
			"3) 技能目录 /home/agent/.dsh/skills（只提供怎么用，真正的调用能力来自上面的 CLI）。" +
			"【规则一：巡检报告只能来自 SRE】用户说\"巡检报告\"\"巡检结果\"或要看集群健康报告时，必须执行 sre-cli report，" +
			"把返回的报告原样呈现（含 inspection_id 与时间）；报告时间超过 30 分钟或用户要\"最新\"时，先执行 sre-cli inspect 再 report。" +
			"严禁用自己 kubectl/dce 查到的内容冒充巡检报告。" +
			"【规则二：自检必须声明】用户没有要报告、只是让你查某个 Pod/指标时，可以自己排查，" +
			"但必须注明\"以下是我的临时自检，不是 SRE 巡检报告\"，并说明信息来源（kubectl / DCE API / SRE）。" +
			"【规则三：别猜权限】不要事先声称没有权限，先执行；命令真报错就把原始错误原样贴出来（如 403 Forbidden）。" +
			"【规则四：别编输出】命令输出、HTTP 状态码、报告字段都要来自真实执行。用户请求："

		parts := []map[string]any{{"kind": "text", "text": preamble + in.Message + credBlock(r)}}
		parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"sreToken": bearerRaw(r)}})
		if kc, err := dceKubeconfig(bearerRaw(r)); err == nil {
			parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"kubeconfig": kc}})
		} else {
			log.Printf("identity=%s dce kubeconfig failed: %v", id, err)
		}

		ctxID := sessionMapGet(sid)
		rememberCtxOwner(ctxID, id)
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "poc-" + sid, "method": "SendMessage",
			"params": map[string]any{
				"message": map[string]any{
					"role": "ROLE_USER", "kind": "message",
					"messageId": "poc-" + sid + "-" + strconv.FormatInt(time.Now().UnixNano(), 36), "parts": parts,
				},
				"configuration": map[string]any{"acceptedOutputModes": []string{"text"}},
			},
		})
		if ctxID != "" {
			payload, _ = applyContext(payload, ctxID)
		}

		resp, err := doUpstream(r.Context(), client, upstream, r, id, sid, payload)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "agent unavailable: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if resp.StatusCode != 200 {
			writeJSON(w, resp.StatusCode, map[string]string{"error": strings.TrimSpace(string(raw))})
			return
		}
		if ctxID == "" {
			if c := a2aContextID(raw); c != "" {
				sessionMapSet(sid, c)
				rememberCtxOwner(c, id)
			}
		}
		text := a2aText(raw)
		if text == "" {
			// 上游可能在 200 里带 JSON-RPC error；把真实原因透给调用方，别显示空白。
			text = "agent 没有返回文本：" + strings.TrimSpace(string(raw[:min(len(raw), 400)]))
		}
		writeJSON(w, 200, map[string]string{"session_id": sid, "answer": text})
	})

	if sessionEpoch != "" {
		log.Printf("SESSION_EPOCH=%q 已废弃，被忽略；换会话请用新的 conversation_id", sessionEpoch)
	}
	log.Printf("adapter listening on %s -> %s", addr, upstream)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// dceKubeconfig 用当前用户的 DCE JWT 换 1 小时 kubeconfig（老 POC 的逻辑），
// 放在适配器里做：沙箱里既不需要 JWT，也不需要为 DCE 单独放出网。
type dceCacheEntry struct {
	kubeconfig string
	expires    time.Time
	inflight   chan struct{}
}

var (
	dceURL       = env("DCE_URL", "http://10.31.223.20:31284")
	dceCluster   = env("DCE_CLUSTER", "kpanda-global-cluster")
	dceAPIServer = env("DCE_APISERVER", "https://10.31.223.20:6443")
	dceMu        sync.Mutex
	dceCache     = map[string]*dceCacheEntry{}
)

func dceKubeconfig(jwt string) (string, error) {
	key := sessionID(jwt) // 只用作缓存键，避免存明文 JWT 派生键冲突
	dceMu.Lock()
	if e := dceCache[key]; e != nil && time.Now().Before(e.expires) {
		kc := e.kubeconfig
		dceMu.Unlock()
		return kc, nil
	}
	e := &dceCacheEntry{inflight: make(chan struct{})}
	dceCache[key] = e
	dceMu.Unlock()

	kc, err := fetchDCEKubeconfig(jwt)
	dceMu.Lock()
	if err != nil {
		delete(dceCache, key)
	} else {
		e.kubeconfig, e.expires = kc, time.Now().Add(50*time.Minute)
	}
	close(e.inflight)
	dceMu.Unlock()
	return kc, err
}

func fetchDCEKubeconfig(jwt string) (string, error) {
	body, _ := json.Marshal(map[string]any{"cluster": dceCluster, "expirationSeconds": 3600})
	url := dceURL + "/apis/kpanda.io/v1alpha1/clusters/" + dceCluster + "/clustercert"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	c := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("dce clustercert http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))[:min(len(raw), 160)])
	}
	var out struct {
		KubeConfigString string `json:"kubeConfigString"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || strings.TrimSpace(out.KubeConfigString) == "" {
		return "", errors.New("dce response missing kubeConfigString")
	}
	dec, err := base64.StdEncoding.DecodeString(out.KubeConfigString)
	if err != nil {
		return "", err
	}
	kc := string(dec)
	if !strings.Contains(kc, "apiVersion:") || !strings.Contains(kc, "kind: Config") {
		return "", errors.New("invalid kubeconfig from dce")
	}
	// DCE 下发的是集群内地址，本机 OrbStack 不可达；换成实测可达的地址（同老 POC）
	kc = regexp.MustCompile(`(?m)^([ \t]*server:[ \t]*)https://10\.233\.0\.1:443[ \t]*$`).ReplaceAllString(kc, "${1}"+dceAPIServer)
	return kc, nil
}

func bearer(v *verifier, r *http.Request) (string, string, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", "", errors.New("missing bearer token")
	}
	return v.verify(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")))
}

// forceSession 把 params.contextId 与 params.message.contextId 都改成服务端推导值，
// 并返回被丢弃的客户端值（用于审计）。
func forceSession(body []byte, sid string) (out []byte, forged string, err error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", err
	}
	params, ok := doc["params"].(map[string]any)
	if !ok {
		return nil, "", errors.New("missing params")
	}
	if cur, ok := params["contextId"].(string); ok && cur != "" && cur != sid {
		forged = cur
	}
	params["contextId"] = sid
	if msg, ok := params["message"].(map[string]any); ok {
		if cur, ok := msg["contextId"].(string); ok && cur != "" && cur != sid && forged == "" {
			forged = cur
		}
		msg["contextId"] = sid
	}
	rewritten, err := json.Marshal(doc)
	return rewritten, forged, err
}

func bearerRaw(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// doUpstream 把改写后的 A2A 请求转发给 kagent 的 sandbox 入口。
func doUpstream(ctx context.Context, client *http.Client, upstream string, r *http.Request, identity, sid string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// kagent 1.0 只服务 A2A 1.0；不声明版本会被判成 0.3 并拒绝。
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	// 仅供上游日志/审计；上游不应据此鉴权。
	// kagent 1.0 用 X-User-Id 区分会话归属（每个 DCE 用户一个分区）。
	req.Header.Set("X-User-Id", identity)
	req.Header.Set("X-Sandbox-Identity", identity)
	req.Header.Set("X-Sandbox-Session", sid)
	return client.Do(req)
}

// a2aText 从 A2A 响应里取出回复文本：先 artifacts，再最后一条 agent 消息。
func a2aText(raw []byte) string {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	res := asMap(doc["result"])
	if res == nil {
		return ""
	}
	// A2A 1.0 把结果放在 result.task 里；0.3 直接在 result。
	for _, holder := range []map[string]any{res, asMap(res["task"])} {
		if holder == nil {
			continue
		}
		if arts, ok := holder["artifacts"].([]any); ok {
			for _, a := range arts {
				if t := partsText(asMap(a)["parts"]); strings.TrimSpace(t) != "" {
					return t
				}
			}
		}
	}
	if task := asMap(res["task"]); task != nil {
		if t := lastAgentText(task["history"]); t != "" {
			return t
		}
		if st := asMap(task["status"]); st != nil {
			if m := asMap(st["message"]); m != nil {
				if t := partsText(m["parts"]); t != "" {
					return t
				}
			}
		}
	}
	if t := lastAgentText(res["history"]); t != "" {
		return t
	}
	if st := asMap(res["status"]); st != nil {
		if m := asMap(st["message"]); m != nil {
			return partsText(m["parts"])
		}
	}
	return ""
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func lastAgentText(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	for i := len(arr) - 1; i >= 0; i-- {
		m := asMap(arr[i])
		if m == nil {
			continue
		}
		if role, _ := m["role"].(string); role == "ROLE_AGENT" || role == "agent" {
			if t := partsText(m["parts"]); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// isSendNotAccepted 判断首帧是否为「Session 还在拉起，稍后重试」。
func isSendNotAccepted(frame []byte) bool {
	t := string(frame)
	return strings.Contains(t, "KAGENT_SEND_NOT_ACCEPTED") || strings.Contains(t, "was not accepted")
}
