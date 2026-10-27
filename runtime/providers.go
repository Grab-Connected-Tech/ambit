package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"keystone.dev/keystone/model"
)

// providersFromConfig builds the providers ks.yaml configures under effects (§10.3,
// §18.2). Every kind has one: an unconfigured kind logs instead of failing, so a new app
// runs before anyone has credentials, and HTTP always uses the guarded client.
//
//	effects:
//	  email:   {provider: smtp, host: smtp.example.com, port: "587", username: u, password: secret:smtp, from: app@example.com, templates: templates}
//	  webhook: {provider: signed, endpoint: https://hooks.example.com/in, secret: secret:webhook}
//	  storage: {provider: s3, endpoint: https://s3.eu-west-1.amazonaws.com, region: eu-west-1, accessKey: ..., secretKey: secret:s3}
func providersFromConfig(c *Config, log Logger) (map[model.EffectKind]Provider, error) {
	out := map[model.EffectKind]Provider{}
	for _, k := range []model.EffectKind{model.EffectEmail, model.EffectSMS, model.EffectWebhook, model.EffectStorage, model.EffectEnqueue} {
		out[k] = LogProvider{Kind: k, Log: log}
	}
	out[model.EffectHTTP] = &HTTPProvider{}
	for name, s := range c.Effects {
		kind := model.EffectKind(name)
		switch p := s["provider"]; {
		case p == "" || p == "log":
		case kind == model.EffectEmail && p == "smtp":
			port, _ := strconv.Atoi(orStr(s["port"], "587"))
			if s["host"] == "" || s["from"] == "" {
				return nil, errors.New("effects.email: smtp needs host and from")
			}
			out[kind] = &SMTPProvider{Host: s["host"], Port: port, Username: s["username"], Password: s["password"], From: s["from"], Templates: s["templates"]}
		case kind == model.EffectWebhook && p == "signed":
			if s["endpoint"] == "" || s["secret"] == "" {
				return nil, errors.New("effects.webhook: signed needs endpoint and secret")
			}
			out[kind] = &WebhookProvider{Endpoint: s["endpoint"], Secret: []byte(s["secret"])}
		case kind == model.EffectStorage && p == "s3":
			if s["endpoint"] == "" || s["accessKey"] == "" || s["secretKey"] == "" {
				return nil, errors.New("effects.storage: s3 needs endpoint, accessKey and secretKey")
			}
			out[kind] = &S3Provider{Endpoint: s["endpoint"], Region: orStr(s["region"], "us-east-1"), AccessKey: s["accessKey"], SecretKey: s["secretKey"]}
		default:
			return nil, fmt.Errorf("effects.%s: no built-in provider %q (register a community provider in code)", name, p)
		}
	}
	return out, nil
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ---- email ----

// Email is the payload of an email effect: an action passes the recipient and either
// a subject and body, or data for the declared template.
type Email struct {
	To      []string       `json:"to"`
	Subject string         `json:"subject,omitempty"`
	Text    string         `json:"text,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// SMTPProvider sends email through an SMTP relay with STARTTLS when offered.
type SMTPProvider struct {
	Host, Username, Password, From string
	Port                           int
	// Templates is the directory of declared templates: <name>.txt holds a first line
	// "Subject: ..." and a text/template body rendered with the payload's data.
	Templates string
	send      func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func (p *SMTPProvider) Name() string { return "smtp" }

func (p *SMTPProvider) Retryable(err error) bool { return !errors.Is(err, errPermanent) }

func (p *SMTPProvider) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	msg, to, err := p.message(e)
	if err != nil {
		return EffectResult{}, Permanent(err)
	}
	var auth smtp.Auth
	if p.Username != "" {
		auth = smtp.PlainAuth("", p.Username, p.Password, p.Host)
	}
	send := p.send
	if send == nil {
		send = smtp.SendMail
	}
	if err := send(net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), auth, p.From, to, msg); err != nil {
		return EffectResult{}, err
	}
	return EffectResult{}, nil
}

// message renders the RFC 5322 message. The idempotency key becomes the Message-ID, so
// a retried send is recognisable as the same message downstream.
func (p *SMTPProvider) message(e EffectCall) ([]byte, []string, error) {
	var m Email
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		return nil, nil, fmt.Errorf("email payload: %w", err)
	}
	if len(m.To) == 0 {
		return nil, nil, errors.New("email payload has no recipient")
	}
	subject, body := m.Subject, m.Text
	if e.Template != "" {
		var err error
		if subject, body, err = p.render(e.Template, m.Data); err != nil {
			return nil, nil, err
		}
	}
	for _, v := range append(append([]string{}, m.To...), subject, p.From) {
		if strings.ContainsAny(v, "\r\n") {
			return nil, nil, errors.New("email header contains a line break") // header injection
		}
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@keystone>\r\n", p.From, strings.Join(m.To, ", "), subject, e.IdempotencyKey)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.Bytes(), m.To, nil
}

func (p *SMTPProvider) render(name string, data map[string]any) (string, string, error) {
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", "", fmt.Errorf("template name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(orStr(p.Templates, "templates"), name+".txt"))
	if err != nil {
		return "", "", fmt.Errorf("email template %s: %w", name, err)
	}
	t, err := template.New(name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return "", "", err
	}
	first, rest, _ := strings.Cut(out.String(), "\n")
	subject, ok := strings.CutPrefix(first, "Subject: ")
	if !ok {
		return "", "", fmt.Errorf("template %s must start with a Subject: line", name)
	}
	return strings.TrimSpace(subject), rest, nil
}

// ---- signed webhooks ----

// WebhookProvider posts each webhook effect to the subscriber endpoint, signed with
// HMAC-SHA256 over the timestamp and body, the scheme receivers verify with:
//
//	Keystone-Signature: t=<unix seconds>,v1=<hex hmac(secret, "<t>.<body>")>
type WebhookProvider struct {
	Endpoint string
	Secret   []byte
	Client   *http.Client
	// allowPrivate lets tests post to a loopback server; the SSRF guard applies otherwise.
	allowPrivate bool
	now          func() time.Time
}

func (p *WebhookProvider) Name() string { return "webhook" }

func (p *WebhookProvider) Retryable(err error) bool { return !errors.Is(err, errPermanent) }

// Sign is the signature header for a body at a time.
func (p *WebhookProvider) Sign(at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, p.Secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (p *WebhookProvider) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" && !p.allowPrivate {
		return EffectResult{}, Permanent(fmt.Errorf("webhook endpoint must be https: %q", p.Endpoint))
	}
	if !p.allowPrivate {
		if err := AssertPublicHost(ctx, u.Hostname()); err != nil {
			return EffectResult{}, Permanent(err)
		}
	}
	body, _ := json.Marshal(map[string]any{"topic": e.Topic, "id": e.IdempotencyKey, "action": e.Action, "data": e.Payload})
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(body))
	if err != nil {
		return EffectResult{}, Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Keystone-Topic", e.Topic)
	req.Header.Set("Idempotency-Key", e.IdempotencyKey)
	req.Header.Set("Keystone-Signature", p.Sign(now(), body))
	return doLimited(p.Client, req, e)
}

// ---- S3-compatible storage ----

// Object is the payload of a storage effect.
type Object struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType,omitempty"`
	Data        []byte `json:"data"` // base64 in JSON
}

// S3Provider writes objects to an S3-compatible store with a SigV4-signed PUT, using
// path-style addressing (endpoint/bucket/key), which every S3-compatible store accepts.
type S3Provider struct {
	Endpoint, Region, AccessKey, SecretKey string
	Client                                 *http.Client
	now                                    func() time.Time
}

func (p *S3Provider) Name() string { return "s3" }

func (p *S3Provider) Retryable(err error) bool { return !errors.Is(err, errPermanent) }

func (p *S3Provider) Execute(ctx context.Context, e EffectCall) (EffectResult, error) {
	var o Object
	if err := json.Unmarshal(e.Payload, &o); err != nil || o.Key == "" || e.Bucket == "" {
		return EffectResult{}, Permanent(fmt.Errorf("storage payload needs a key and the effect a bucket: %v", err))
	}
	target := strings.TrimRight(p.Endpoint, "/") + "/" + url.PathEscape(e.Bucket) + "/" + escapeKey(o.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(o.Data))
	if err != nil {
		return EffectResult{}, Permanent(err)
	}
	if o.ContentType != "" {
		req.Header.Set("Content-Type", o.ContentType)
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	SignV4(req, o.Data, p.AccessKey, p.SecretKey, p.Region, "s3", now())
	return doLimited(p.Client, req, e)
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// SignV4 adds AWS Signature Version 4 headers to a request whose body is payload.
func SignV4(req *http.Request, payload []byte, accessKey, secretKey, region, service string, at time.Time) {
	at = at.UTC()
	amzDate, day := at.Format("20060102T150405Z"), at.Format("20060102")
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", amzDate)
	if service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	key := hmacSHA256([]byte("AWS4"+secretKey), day)
	for _, part := range []string{region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// doLimited sends a request and classifies the response: 5xx and network errors retry,
// 4xx is permanent, and the body is read up to 64 KiB.
func doLimited(c *http.Client, req *http.Request, e EffectCall) (EffectResult, error) {
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return EffectResult{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests:
		return EffectResult{}, fmt.Errorf("%s %s: %d", req.Method, req.URL.Host, res.StatusCode)
	case res.StatusCode >= 400:
		return EffectResult{}, Permanent(fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Host, res.StatusCode, bytes.TrimSpace(body)))
	}
	if !json.Valid(body) {
		body, _ = json.Marshal(base64.StdEncoding.EncodeToString(body))
	}
	return EffectResult{Body: body}, nil
}
