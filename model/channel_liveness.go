package model

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Channel link liveness state.
//
// Historically this file also gated channel selection with a TCP connect probe
// on the request path. That approach was removed: a TCP blackhole (host powered
// off, no RST) still burns the dial timeout on every selection, a port that is
// open does not prove the inference server is actually serving, and a brief
// 30-second suspension let the channel be re-selected almost immediately. The
// replacement is the LAN supervisor in controller/channel_local_probe.go, which
// pings the host first (bounded ICMP), then checks TCP and an HTTP readiness
// endpoint, and auto-disables / auto-recovers the channel accordingly.
//
// This file now only owns the lightweight link-state cache used by the admin UI
// and the plain IP-literal classification the supervisor relies on.

// channelLinkState is the last measured reachability of a channel's upstream.
type channelLinkState struct {
	alive     bool
	checkedAt time.Time
	rtt       time.Duration
}

var (
	channelLinkMu    sync.Mutex
	channelLinkStore = map[int]*channelLinkState{}
)

// channelLinkAddr returns the host:port of a channel's upstream, or "" when the
// base URL is absent or not an IP literal. Only IP-literal endpoints (LAN/GPU
// nodes, a local llama.cpp) are considered; public hostnames are left alone to
// avoid paying DNS and to avoid misreading a resolver hiccup as a dead node.
func channelLinkAddr(ch *Channel) string {
	if ch == nil || ch.BaseURL == nil {
		return ""
	}
	raw := strings.TrimSpace(*ch.BaseURL)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// LocalChannelAddr returns the host:port of an IP-literal channel upstream, or
// "" when the channel is not an IP-literal endpoint.
func LocalChannelAddr(ch *Channel) string {
	return channelLinkAddr(ch)
}

// IsLocalIPChannel reports whether a channel targets an IP-literal upstream and
// is therefore a candidate for the LAN supervisor.
func IsLocalIPChannel(ch *Channel) bool {
	return channelLinkAddr(ch) != ""
}

// SetChannelLinkState records the most recent link verdict for a channel so the
// admin UI can display it. It never influences routing or scoring.
func SetChannelLinkState(channelId int, alive bool, rtt time.Duration) {
	if channelId == 0 {
		return
	}
	channelLinkMu.Lock()
	channelLinkStore[channelId] = &channelLinkState{alive: alive, checkedAt: time.Now(), rtt: rtt}
	channelLinkMu.Unlock()
}

// ChannelLinkSnapshot returns the cached link state for the admin UI.
// checked is false when nothing has been measured yet.
func ChannelLinkSnapshot(channelId int) (alive bool, rttMs float64, checked bool) {
	channelLinkMu.Lock()
	defer channelLinkMu.Unlock()
	st := channelLinkStore[channelId]
	if st == nil {
		return false, 0, false
	}
	return st.alive, float64(st.rtt.Microseconds()) / 1000.0, true
}

// applyLinkSnapshot copies the cached link verdict onto a health snapshot.
func applyLinkSnapshot(s *HealthSnapshot, channelId int) {
	if s == nil {
		return
	}
	s.LinkReachable, s.LinkRTTMs, s.LinkChecked = ChannelLinkSnapshot(channelId)
}
