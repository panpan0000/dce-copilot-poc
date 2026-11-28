// Package dsh bridges the DeepSeek Harness (DSH) HTTP RPC server to kagent's
// runtime event model.
package dsh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
)

const (
	pollInterval = 300 * time.Millisecond
	turnTimeout  = 10 * time.Minute
)

var _ runtimea2a.Runner = (*Runner)(nil)

// Runner drives one DSH session per kagent context.
type Runner struct {
	endpoint string
	cwd      string
	client   *http.Client

	mu        sync.Mutex
	byContext map[string]string // kagent contextId -> dsh sessionId
}

// New returns a runner talking to the DSH web profile.
func New(endpoint, cwd string) *Runner {
	return &Runner{
		endpoint:  strings.TrimRight(endpoint, "/"),
		cwd:       cwd,
		client:    &http.Client{Timeout: 60 * time.Second},
		byContext: map[string]string{},
	}
}

// Close is a no-op: DSH owns its own lifecycle.
func (r *Runner) Close() error { return nil }

// Validate checks DSH reachability.
func (r *Runner) Validate(ctx context.Context) error {
	_, err := r.rpc(ctx, "session.create", map[string]any{"cwd": r.cwd})
	return err
}

type event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type assistantMessage struct {
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// Run executes one turn against DSH and streams assistant text as deltas.
func (r *Runner) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	outcome, err := r.run(ctx, turn, sink)
	if err != nil {
		// 沙箱内日志不可见，把失败原因作为回答暴露出来，便于定位
		_ = sink.TextDelta(runtime.TextDelta{Text: "[dsh] " + err.Error()})
		return runtime.Outcome{}, nil
	}
	return outcome, nil
}

func (r *Runner) run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if strings.TrimSpace(turn.Prompt) == "" {
		return runtime.Outcome{}, fmt.Errorf("dsh prompt is required")
	}
	sid, err := r.session(ctx, turn.ContextID)
	if err != nil {
		return runtime.Outcome{}, err
	}
	if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: sid}); err != nil {
		return runtime.Outcome{}, err
	}
	baseline, err := r.maxSeq(ctx, sid)
	if err != nil {
		return runtime.Outcome{}, err
	}
	if _, err := r.rpc(ctx, "session.prompt", map[string]any{
		"sessionId": sid,
		"mode":      "queue",
		"content":   []map[string]any{{"type": "text", "text": turn.Prompt}},
	}); err != nil {
		return runtime.Outcome{}, err
	}

	deadline := time.Now().Add(turnTimeout)
	sent := ""
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return runtime.Outcome{}, ctx.Err()
		case <-time.After(pollInterval):
		}
		events, err := r.history(ctx, sid)
		if err != nil {
			return runtime.Outcome{}, err
		}
		text := chunksText(events, baseline)
		if text == "" {
			text = lastText(events, baseline)
		}
		if strings.HasPrefix(text, sent) && len(text) > len(sent) {
			if err := sink.TextDelta(runtime.TextDelta{Text: text[len(sent):]}); err != nil {
				return runtime.Outcome{}, err
			}
			sent = text
		}
		if ended(events, baseline) {
			if text := lastText(events, baseline); strings.HasPrefix(text, sent) && len(text) > len(sent) {
				if err := sink.TextDelta(runtime.TextDelta{Text: text[len(sent):]}); err != nil {
					return runtime.Outcome{}, err
				}
				sent = text
			}
			if sent == "" {
				// 回合结束但没有任何文本：把事件尾巴回给对话，便于定位 DSH 侧问题
				var tail []string
				for _, e := range events {
					if e.Seq > baseline {
						tail = append(tail, fmt.Sprintf("%s:%s", e.Type, clip(string(e.Data), 120)))
					}
				}
				if len(tail) > 6 {
					tail = tail[len(tail)-6:]
				}
				_ = sink.TextDelta(runtime.TextDelta{Text: "[dsh] 本轮无文本输出；事件尾巴: " + strings.Join(tail, " | ")})
			}
			return runtime.Outcome{}, nil
		}
	}
	return runtime.Outcome{}, fmt.Errorf("dsh turn timed out after %s", turnTimeout)
}

func (r *Runner) session(ctx context.Context, contextID string) (string, error) {
	r.mu.Lock()
	sid, ok := r.byContext[contextID]
	r.mu.Unlock()
	if ok {
		return sid, nil
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	// DSH 冷启动要几秒到几十秒：这里等而不是直接失败
	deadline := time.Now().Add(90 * time.Second)
	var err error
	for {
		if err = r.call(ctx, "session.create", map[string]any{"cwd": r.cwd}, &out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("dsh session.create: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("dsh session.create returned no sessionId")
	}
	r.mu.Lock()
	r.byContext[contextID] = out.SessionID
	r.mu.Unlock()
	return out.SessionID, nil
}

func (r *Runner) maxSeq(ctx context.Context, sid string) (int64, error) {
	events, err := r.history(ctx, sid)
	if err != nil {
		return 0, err
	}
	var max int64 = -1
	for _, e := range events {
		if e.Seq > max {
			max = e.Seq
		}
	}
	return max, nil
}

func (r *Runner) history(ctx context.Context, sid string) ([]event, error) {
	// DSH 返回的每条是 {"event": {...}}；这里兼容扁平形式
	var out struct {
		Events []struct {
			Event *event `json:"event"`
			Type  string `json:"type"`
			Seq   int64  `json:"seq"`
			Data  json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if err := r.call(ctx, "session.history", map[string]any{"sessionId": sid, "maxMessages": 6}, &out); err != nil {
		return nil, err
	}
	events := make([]event, 0, len(out.Events))
	for _, it := range out.Events {
		if it.Event != nil {
			events = append(events, *it.Event)
			continue
		}
		events = append(events, event{Type: it.Type, Seq: it.Seq, Data: it.Data})
	}
	return events, nil
}

// chunksText 把 assistant/chunk 里的增量文本拼起来（DSH 的流式片段）。
func chunksText(events []event, baseline int64) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type != "assistant/chunk" || e.Seq <= baseline {
			continue
		}
		var d struct {
			Chunk struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"chunk"`
		}
		if err := json.Unmarshal(d_remarshal(e.Data), &d); err != nil {
			continue
		}
		if d.Chunk.Text != "" {
			b.WriteString(d.Chunk.Text)
		}
	}
	return b.String()
}

func d_remarshal(raw json.RawMessage) json.RawMessage { return raw }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func lastText(events []event, baseline int64) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != "assistant/message" || e.Seq <= baseline {
			continue
		}
		var m assistantMessage
		if err := json.Unmarshal(e.Data, &m); err != nil {
			continue
		}
		var b strings.Builder
		for _, c := range m.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return ""
}

func ended(events []event, baseline int64) bool {
	for _, e := range events {
		if e.Type == "turn/end" && e.Seq > baseline {
			return true
		}
	}
	return false
}

func (r *Runner) rpc(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	var out json.RawMessage
	err := r.call(ctx, method, payload, &out)
	return out, err
}

func (r *Runner) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(map[string]any{
		"type":   "client-request",
		"rpcId":  newID(),
		"method": method,
		"payload": payload,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("dsh %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dsh %s http %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw))[:min(len(raw), 160)])
	}
	var envelope struct {
		Result struct {
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("dsh %s decode: %w", method, err)
	}
	if !envelope.Result.OK {
		return fmt.Errorf("dsh %s failed: %s", method, envelope.Result.Error)
	}
	if out != nil && len(envelope.Result.Value) > 0 {
		return json.Unmarshal(envelope.Result.Value, out)
	}
	return nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
