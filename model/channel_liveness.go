package model

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Laisky/zap"

	"github.com/Laisky/one-api/common/logger"
)

// Channel link liveness probing.
//
// The health engine scores channels from live relay traffic and real request
// probes. That leaves one gap: a channel whose host has simply disappeared
// (powered off, cable pulled, blackholed route) takes the full OS connect
// timeout to fail — on Linux, tcp_syn_retries=6 means ~127s of silent SYN
// retransmits. By the time the relay reports the failure the caller has often
// already cancelled, and a cancelled request is classified as caller-side and
// therefore neither retried nor suspended. The dead channel — if it is the
// only member of the highest-priority tier — then stays the candidate forever,
// because selectByHealthBand returns a lone candidate unconditionally.
//
// TCP link probing closes that gap. Before a candidate is returned, its
// upstream host:port is connect-checked with a short budget; an unreachable
// link is suspended briefly (so every selection path skips it) without ever
// touching the channel's health score. Reachability is a hard gate; latency is
// judged purely from relay traffic.
//
// Only IP-literal endpoints are probed. GPU/LAN nodes are configured as IP
// literals, while public providers use hostnames; skipping hostname endpoints
// avoids paying DNS on the selection path and avoids gating out a public
// provider over a transient resolver hiccup.

const (
	// channelLinkDialTimeout bounds a single TCP connect probe.
	channelLinkDialTimeout = 2 * time.Second
	// channelLinkAliveTTL is how long a successful probe is trusted.
	channelLinkAliveTTL = 15 * time.Second
	// channelLinkDeadTTL is how long a failed probe is trusted. Shorter than
	// the alive TTL so a recovered link is noticed promptly.
	channelLinkDeadTTL = 10 * time.Second
	// channelLinkSuspendDuration briefly removes an unreachable channel from
	// selection. It self-heals: once the link returns the probe succeeds and
	// the channel rejoins as soon as the suspension expires.
	channelLinkSuspendDuration = 30 * time.Second
)

type channelLinkState struct {
	alive     bool
	checkedAt time.Time
	rtt       time.Duration
}

var (
	channelLinkMu    sync.Mutex
	channelLinkStore = map[int]*channelLinkState{}

	// suspendUnreachableLink is a seam for tests. Production suspends the
	// ability for a short, self-healing window.
	suspendUnreachableLink = func(group, model string, channelId int) error {
		return SuspendAbility(context.Background(), group, model, channelId, channelLinkSuspendDuration)
	}
)

// channelLinkAddr returns the host:port to connect-check for a channel, or ""
// when the channel has no IP-literal endpoint worth probing.
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
	// Only IP literals are probed (LAN/GPU nodes); public hostnames are left
	// to the relay's own timeout handling.
	if net.ParseIP(host) == nil {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// channelLinkProbe performs one TCP connect and returns reachability + RTT.
func channelLinkProbe(ch *Channel, timeout time.Duration) (bool, time.Duration) {
	addr := channelLinkAddr(ch)
	if addr == "" {
		// Nothing to probe: treat as reachable so an unprobeable channel is
		// never gated out.
		return true, 0
	}
	if timeout <= 0 {
		timeout = channelLinkDialTimeout
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	rtt := time.Since(start)
	if err != nil {
		return false, rtt
	}
	_ = conn.Close()
	return true, rtt
}

// ChannelLinkAlive returns the cached link verdict for a channel, probing when
// the cached verdict is stale. Safe for concurrent use.
func ChannelLinkAlive(ch *Channel) bool {
	alive, _, _ := channelLinkCheck(ch)
	return alive
}

func channelLinkCheck(ch *Channel) (alive bool, rtt time.Duration, probed bool) {
	if ch == nil {
		return true, 0, false
	}
	if channelLinkAddr(ch) == "" {
		return true, 0, false
	}
	now := time.Now()
	channelLinkMu.Lock()
	if st := channelLinkStore[ch.Id]; st != nil {
		ttl := channelLinkAliveTTL
		if !st.alive {
			ttl = channelLinkDeadTTL
		}
		if now.Sub(st.checkedAt) < ttl {
			alive, rtt = st.alive, st.rtt
			channelLinkMu.Unlock()
			return alive, rtt, true
		}
	}
	channelLinkMu.Unlock()

	alive, rtt = channelLinkProbe(ch, 0)

	channelLinkMu.Lock()
	channelLinkStore[ch.Id] = &channelLinkState{alive: alive, checkedAt: now, rtt: rtt}
	channelLinkMu.Unlock()
	return alive, rtt, true
}

// ChannelLinkSnapshot returns the cached link state for the admin UI without
// triggering a probe. checked is false when nothing has been measured yet.
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

// filterLinkDead drops channels whose upstream link is currently unreachable,
// briefly suspending them so every selection path skips them. Reachability is
// a hard gate and never moves the health score.
//
// The last candidate is never dropped by this filter alone: a total outage
// must surface as a relay error, not as "no channel available". Channels that
// are already suspended are excluded without probing or re-suspending.
func filterLinkDead(group, model string, channels []*Channel) []*Channel {
	if len(channels) == 0 {
		return channels
	}

	alive := make([]bool, len(channels))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, ch := range channels {
		if ch == nil {
			continue
		}
		if IsChannelSuspendedInCache(ch.Id) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ch *Channel) {
			defer wg.Done()
			defer func() { <-sem }()
			alive[i] = ChannelLinkAlive(ch)
		}(i, ch)
	}
	wg.Wait()

	kept := make([]*Channel, 0, len(channels))
	var dead []*Channel
	for i, ch := range channels {
		if ch == nil {
			continue
		}
		if alive[i] {
			kept = append(kept, ch)
			continue
		}
		dead = append(dead, ch)
	}
	if len(kept) == 0 {
		// Everything looks unreachable; do not turn a fleet-wide outage into
		// a selection error. Fall through with the original candidates.
		return channels
	}
	for _, ch := range dead {
		if IsChannelSuspendedInCache(ch.Id) {
			continue
		}
		if err := suspendUnreachableLink(group, model, ch.Id); err != nil {
			logger.Logger.Warn("failed to suspend unreachable channel link",
				zap.Int("channel_id", ch.Id),
				zap.String("channel_name", ch.Name),
				zap.Error(err))
			continue
		}
		logger.Logger.Warn("channel link unreachable; suspended briefly",
			zap.Int("channel_id", ch.Id),
			zap.String("channel_name", ch.Name),
			zap.Duration("duration", channelLinkSuspendDuration))
	}
	return kept
}
