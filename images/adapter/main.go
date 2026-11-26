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

func main() {
	var (
		addr     = env("ADAPTER_ADDR", ":9099")
		jwksPath = env("JWKS_FILE", "/etc/adapter/jwks.json")
		issuer   = env("JWT_ISSUER", "")
		audience = env("JWT_AUDIENCE", "")
		upstream = env("UPSTREAM", "http://kagent-controller.kagent.svc.cluster.local:8083/api/a2a-sandboxes/kagent/substrate-demo/")
	)
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
		sid := sessionID(id)

		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		rewritten, forged, err := forceSession(body, sid)
		if err != nil {
			http.Error(w, "invalid json-rpc body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if forged != "" {
			log.Printf("identity=%s ignored client contextId=%q", id, forged)
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
		if in.ConversationID != "" {
			if !convIDRe.MatchString(in.ConversationID) {
				http.Error(w, "invalid conversation_id", http.StatusBadRequest)
				return
			}
			sid = sessionIDFor(id, in.ConversationID)
		}
		parts := []map[string]any{{"kind": "text", "text": in.Message}}
		parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"sreToken": bearerRaw(r)}})
		if kc, kerr := dceKubeconfig(bearerRaw(r)); kerr == nil {
			parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"kubeconfig": kc}})
		}
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "str-" + sid, "method": "message/stream",
			"params": map[string]any{
				"contextId": sid,
				"message": map[string]any{"role": "user", "kind": "message", "contextId": sid,
					"messageId": "str-" + sid, "parts": parts},
				"configuration": map[string]any{"acceptedOutputModes": []string{"text"}},
			},
		})
		resp, err := doUpstream(r.Context(), client, upstream, r, id, sid, payload)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "agent unavailable: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Sandbox-Session", sid)
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		emit := func(obj any) {
			raw, _ := json.Marshal(obj)
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev struct {
				Result struct {
					Artifacts []struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"artifacts"`
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				} `json:"result"`
				Error any `json:"error"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
				continue
			}
			if ev.Error != nil {
				emit(map[string]any{"error": ev.Error})
				continue
			}
			for _, a := range ev.Result.Artifacts {
				for _, p := range a.Parts {
					if p.Text != "" {
						emit(map[string]string{"delta": p.Text})
					}
				}
			}
			if ev.Result.Status.State == "completed" {
				emit(map[string]any{"done": true, "session_id": sid})
			}
		}
		emit(map[string]any{"done": true, "session_id": sid})
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

		parts := []map[string]any{{"kind": "text", "text": preamble + in.Message}}
		parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"sreToken": bearerRaw(r)}})
		if kc, err := dceKubeconfig(bearerRaw(r)); err == nil {
			parts = append(parts, map[string]any{"kind": "data", "data": map[string]string{"kubeconfig": kc}})
		} else {
			log.Printf("identity=%s dce kubeconfig failed: %v", id, err)
		}

		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": "poc-" + sid, "method": "message/send",
			"params": map[string]any{
				"contextId": sid,
				"message": map[string]any{
					"role": "user", "kind": "message", "contextId": sid,
					"messageId": "poc-" + sid, "parts": parts,
				},
				"configuration": map[string]any{"acceptedOutputModes": []string{"text"}},
			},
		})

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
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	// 仅供上游日志/审计；上游不应据此鉴权。
	req.Header.Set("X-Sandbox-Identity", identity)
	req.Header.Set("X-Sandbox-Session", sid)
	return client.Do(req)
}

// a2aText 从 A2A 响应里取出回复文本：先 artifacts，再最后一条 agent 消息。
func a2aText(raw []byte) string {
	var doc struct {
		Result struct {
			Artifacts []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
			History []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
			Status struct {
				Message struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, a := range doc.Result.Artifacts {
		for _, p := range a.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	for i := len(doc.Result.History) - 1; i >= 0; i-- {
		if doc.Result.History[i].Role == "agent" {
			for _, p := range doc.Result.History[i].Parts {
				if p.Text != "" {
					return p.Text
				}
			}
		}
	}
	for _, p := range doc.Result.Status.Message.Parts {
		if p.Text != "" {
			return p.Text
		}
	}
	return ""
}
