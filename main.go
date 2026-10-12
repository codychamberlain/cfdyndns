// cfdyndns keeps Cloudflare DNS records pointed at this host's public IP.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const cfAPI = "https://api.cloudflare.com/client/v4"

type config struct {
	token      string
	zoneID     string
	zoneName   string
	records    []string
	recordType string // "A" or "AAAA"
	interval   time.Duration
	ttl        int
	proxied    bool
}

func loadConfig() (*config, error) {
	c := &config{
		token:      os.Getenv("CF_API_TOKEN"),
		zoneID:     os.Getenv("CF_ZONE_ID"),
		zoneName:   os.Getenv("CF_ZONE_NAME"),
		recordType: strings.ToUpper(envOr("CF_RECORD_TYPE", "A")),
		interval:   5 * time.Minute,
		ttl:        1, // 1 = "automatic" in Cloudflare
	}
	for _, r := range strings.Split(os.Getenv("CF_RECORD_NAMES"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.records = append(c.records, r)
		}
	}
	if v := os.Getenv("CF_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 30*time.Second {
			return nil, fmt.Errorf("CF_INTERVAL must be a duration >= 30s, got %q", v)
		}
		c.interval = d
	}
	if v := os.Getenv("CF_TTL"); v != "" {
		if _, err := fmt.Sscan(v, &c.ttl); err != nil {
			return nil, fmt.Errorf("CF_TTL must be an integer, got %q", v)
		}
	}
	c.proxied = os.Getenv("CF_PROXIED") == "true"

	switch {
	case c.token == "":
		return nil, errors.New("CF_API_TOKEN is required")
	case c.zoneID == "" && c.zoneName == "":
		return nil, errors.New("one of CF_ZONE_ID or CF_ZONE_NAME is required")
	case len(c.records) == 0:
		return nil, errors.New("CF_RECORD_NAMES is required (comma-separated FQDNs)")
	case c.recordType != "A" && c.recordType != "AAAA":
		return nil, fmt.Errorf("CF_RECORD_TYPE must be A or AAAA, got %q", c.recordType)
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---- public IP discovery ----

// ipSources return the caller's address as plain text or Cloudflare's trace format.
var ipSources = []string{
	"https://1.1.1.1/cdn-cgi/trace",
	"https://api64.ipify.org",
	"https://icanhazip.com",
	"https://ifconfig.me/ip",
}

var ipSourcesV6 = []string{
	"https://[2606:4700:4700::1111]/cdn-cgi/trace",
	"https://api64.ipify.org",
	"https://icanhazip.com",
	"https://ifconfig.me/ip",
}

// newIPClient returns an HTTP client that only dials over the given family
// ("tcp4" or "tcp6"), so the echo services see the address we want.
func newIPClient(network string) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

func publicIP(ctx context.Context, recordType string) (netip.Addr, error) {
	network, sources := "tcp4", ipSources
	if recordType == "AAAA" {
		network, sources = "tcp6", ipSourcesV6
	}
	client := newIPClient(network)

	var errs []error
	for _, src := range sources {
		ip, err := fetchIP(ctx, client, src)
		if err == nil {
			if (recordType == "A") != ip.Is4() {
				err = fmt.Errorf("got %s, wrong address family", ip)
			} else {
				return ip, nil
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", src, err))
	}
	return netip.Addr{}, errors.Join(errs...)
}

func fetchIP(ctx context.Context, client *http.Client, src string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("User-Agent", "cfdyndns")
	resp, err := client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return netip.Addr{}, err
	}
	text := strings.TrimSpace(string(body))
	// Cloudflare trace output is key=value lines; pick out "ip=".
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "ip="); ok {
			text = v
			break
		}
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(text))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("unparseable response %q", text)
	}
	return ip.Unmap(), nil
}

// ---- Cloudflare API ----

type cfClient struct {
	token string
	http  *http.Client
}

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
	Proxied *bool  `json:"proxied,omitempty"`
}

func (c *cfClient) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfAPI+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var cr cfResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return fmt.Errorf("%s %s: HTTP %d, bad response: %w", method, path, resp.StatusCode, err)
	}
	if !cr.Success {
		msgs := make([]string, 0, len(cr.Errors))
		for _, e := range cr.Errors {
			msgs = append(msgs, fmt.Sprintf("%d: %s", e.Code, e.Message))
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(cr.Result, out)
	}
	return nil
}

func (c *cfClient) zoneID(ctx context.Context, name string) (string, error) {
	var zones []struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("zone %q not found (check the token has Zone:Read)", name)
	}
	return zones[0].ID, nil
}

func (c *cfClient) findRecord(ctx context.Context, zoneID, typ, name string) (*dnsRecord, error) {
	var recs []dnsRecord
	q := url.Values{"type": {typ}, "name": {name}}
	path := "/zones/" + zoneID + "/dns_records?" + q.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &recs); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// ---- main loop ----

type updater struct {
	cfg    *config
	cf     *cfClient
	zoneID string
	lastIP netip.Addr
}

func (u *updater) run(ctx context.Context) error {
	ip, err := publicIP(ctx, u.cfg.recordType)
	if err != nil {
		return fmt.Errorf("discovering public IP: %w", err)
	}
	slog.Debug("discovered public IP", "ip", ip)
	if ip == u.lastIP {
		slog.Debug("IP unchanged", "ip", ip)
		return nil
	}

	if u.zoneID == "" {
		if u.zoneID, err = u.cf.zoneID(ctx, u.cfg.zoneName); err != nil {
			return err
		}
	}

	var failed bool
	for _, name := range u.cfg.records {
		if err := u.sync(ctx, name, ip.String()); err != nil {
			slog.Error("update failed", "record", name, "err", err)
			failed = true
		}
	}
	if failed {
		return errors.New("one or more records failed to update")
	}
	u.lastIP = ip
	return nil
}

func (u *updater) sync(ctx context.Context, name, ip string) error {
	rec, err := u.cf.findRecord(ctx, u.zoneID, u.cfg.recordType, name)
	if err != nil {
		return err
	}
	if rec == nil {
		proxied := u.cfg.proxied
		newRec := dnsRecord{Type: u.cfg.recordType, Name: name, Content: ip, TTL: u.cfg.ttl, Proxied: &proxied}
		if err := u.cf.do(ctx, http.MethodPost, "/zones/"+u.zoneID+"/dns_records", newRec, nil); err != nil {
			return err
		}
		slog.Info("created record", "record", name, "type", u.cfg.recordType, "ip", ip)
		return nil
	}
	if rec.Content == ip {
		slog.Info("record already current", "record", name, "ip", ip)
		return nil
	}
	// PATCH only the content so existing TTL/proxied/comment settings are kept.
	patch := map[string]string{"content": ip}
	if err := u.cf.do(ctx, http.MethodPatch, "/zones/"+u.zoneID+"/dns_records/"+rec.ID, patch, nil); err != nil {
		return err
	}
	slog.Info("updated record", "record", name, "old", rec.Content, "new", ip)
	return nil
}

func main() {
	once := flag.Bool("once", false, "update once and exit")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if *debug {
		opts.Level = slog.LevelDebug
	}
	if os.Getenv("JOURNAL_STREAM") != "" {
		// journald already timestamps every line.
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u := &updater{
		cfg:    cfg,
		cf:     &cfClient{token: cfg.token, http: &http.Client{Timeout: 30 * time.Second}},
		zoneID: cfg.zoneID,
	}

	if *once {
		if err := u.run(ctx); err != nil {
			slog.Error("update failed", "err", err)
			os.Exit(1)
		}
		return
	}

	slog.Info("starting", "records", strings.Join(cfg.records, ","), "type", cfg.recordType, "interval", cfg.interval)
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		if err := u.run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("update failed, will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
		}
	}
}
