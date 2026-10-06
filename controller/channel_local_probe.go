package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Laisky/zap"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/logger"
	"github.com/Laisky/one-api/model"
	"github.com/Laisky/one-api/monitor"
)

// ---------------------------------------------------------------------------
// LAN channel supervisor
//
// A powered-off local node (a workstation hosting llama.cpp, a GPU box) does not
// refuse the TCP connection — it black-holes it, so the OS SYN-retry budget
// (~127s on Linux) elapses before the relay can even report a failure. A port
// that does accept a connection still does not prove the inference server is
// running on it.
//
// This supervisor owns IP-literal (LAN) channels only. On an interval it:
//
//  1. pings the host with a bounded ICMP echo, so a powered-off node is
//     detected in milliseconds instead of black-holing requests;
//  2. connects to the port, to catch "host is up but the server is not bound";
//  3. issues a lightweight GET {baseURL}/v1/models, to catch "something is
//     listening but llama.cpp was never started".
//
// An enabled channel that fails any stage is auto-disabled; an auto-disabled
// channel that passes all three is reset to full score and re-enabled. A
// manually disabled channel is never touched.
// ---------------------------------------------------------------------------

const (
	// localProbeOK is the empty verdict: all three stages passed.
	localProbeOK = ""
	// localProbeHostDown means the host did not answer ICMP (or the TCP host
	// fallback when ICMP is unavailable).
	localProbeHostDown = "host_down"
	// localProbeServiceDown means the host answered but the inference server
	// did not.
	localProbeServiceDown = "service_down"
)

// pingHost sends one ICMP echo and waits for the matching reply. It returns an
// error when the ICMP socket cannot be opened at all (for example the process
// lacks CAP_NET_RAW), which tells the caller to fall back to a TCP host check.
func pingHost(host string, timeout time.Duration) (bool, time.Duration, error) {
	dst := net.ParseIP(host)
	if dst == nil || dst.To4() == nil {
		return false, 0, fmt.Errorf("not an IPv4 literal")
	}
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false, 0, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return false, 0, err
	}
	id := os.Getpid() & 0xffff
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: id, Seq: 1, Data: []byte("one-api-local-probe")},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return false, 0, err
	}
	start := time.Now()
	if _, err := conn.WriteTo(wb, &net.IPAddr{IP: dst}); err != nil {
		return false, 0, err
	}
	reply := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(reply)
		if err != nil {
			return false, time.Since(start), err
		}
		if peer.String() != dst.String() {
			continue
		}
		rm, err := icmp.ParseMessage(1, reply[:n])
		if err != nil {
			continue
		}
		if rm.Type == ipv4.ICMPTypeEchoReply {
			return true, time.Since(start), nil
		}
	}
}

// readinessURL builds the cheap GET used to prove the inference server is up.
// A base URL that already ends in /v1 gets only /models appended.
func readinessURL(ch *model.Channel) string {
	if ch == nil || ch.BaseURL == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(*ch.BaseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// probeLocalChannel classifies an IP-literal channel. It returns a verdict
// (localProbeOK when healthy), a human-readable detail, and the host RTT.
func probeLocalChannel(ch *model.Channel, timeout time.Duration) (string, string, time.Duration) {
	addr := model.LocalChannelAddr(ch)
	if addr == "" {
		return localProbeOK, "", 0
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return localProbeServiceDown, "malformed upstream address", 0
	}

	// Ping first so a powered-off host is noticed as early as possible. A
	// failed ping is confirmed by the TCP connect below rather than trusted on
	// its own: a host whose firewall drops ICMP would otherwise be disabled
	// while the inference server is perfectly reachable.
	var rtt time.Duration
	pingAttempted, pingAlive := false, false
	if alive, pingRTT, pingErr := pingHost(host, timeout); pingErr == nil {
		pingAttempted, pingAlive = true, alive
		if alive {
			rtt = pingRTT
		}
	}

	// TCP connect doubles as the port check and the host-check fallback.
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		if pingAttempted && pingAlive {
			// The host answered but nothing is listening on the port.
			return localProbeServiceDown, "tcp connect refused", rtt
		}
		return localProbeHostDown, "host unreachable (no ICMP reply and tcp connect failed)", rtt
	}
	_ = conn.Close()

	// The port is open; confirm the inference server itself answers.
	u := readinessURL(ch)
	if u == "" {
		return localProbeServiceDown, "no base url", rtt
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return localProbeServiceDown, "invalid readiness url", rtt
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil, // local probes must never traverse an egress proxy
			DialContext:       (&net.Dialer{Timeout: timeout}).DialContext,
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return localProbeServiceDown, "readiness endpoint did not answer", rtt
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return localProbeServiceDown, fmt.Sprintf("readiness endpoint returned %d", resp.StatusCode), rtt
	}
	return localProbeOK, "", rtt
}

// runLocalChannelProbeTick probes every IP-literal channel once and reconciles
// its status. It returns how many channels were probed and how many status
// changes were made.
func runLocalChannelProbeTick() (probed, changed int) {
	timeout := time.Duration(config.LocalChannelProbeTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	channels, err := model.GetAllChannels(0, 0, "all", "", "")
	if err != nil {
		return 0, 0
	}
	lg := logger.Logger.Named("local_channel_supervisor")
	for _, ch := range channels {
		if ch == nil || !model.IsLocalIPChannel(ch) {
			continue
		}
		// Manual disable is a human decision: never probe, never recover.
		if ch.Status != model.ChannelStatusEnabled && ch.Status != model.ChannelStatusAutoDisabled {
			continue
		}
		probed++
		verdict, why, rtt := probeLocalChannel(ch, timeout)
		model.SetChannelLinkState(ch.Id, verdict == localProbeOK, rtt)

		if ch.Status == model.ChannelStatusEnabled && verdict != localProbeOK {
			lg.Warn("local channel auto-disabled by LAN supervisor",
				zap.Int("channel_id", ch.Id),
				zap.String("channel_name", ch.Name),
				zap.String("verdict", verdict),
				zap.String("detail", why),
			)
			monitor.DisableChannel(ch.Id, ch.Name, "local probe: "+why)
			changed++
			continue
		}
		if ch.Status == model.ChannelStatusAutoDisabled && verdict == localProbeOK {
			model.ResetChannelHealthToFull(ch.Id)
			lg.Info("local channel recovered by LAN supervisor",
				zap.Int("channel_id", ch.Id),
				zap.String("channel_name", ch.Name),
			)
			monitor.EnableChannel(ch.Id, ch.Name)
			changed++
		}
	}
	return probed, changed
}

// AutomaticallyProbeLocalChannels runs the LAN supervisor until ctx is done.
func AutomaticallyProbeLocalChannels(ctx context.Context) {
	if !config.LocalChannelProbeEnabled {
		return
	}
	interval := time.Duration(config.LocalChannelProbeIntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	lg := logger.Logger.Named("local_channel_supervisor")
	lg.Info("LAN channel supervisor started", zap.Duration("interval", interval))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probed, changed := runLocalChannelProbeTick()
			lg.Info("LAN channel supervisor tick",
				zap.Int("probed", probed),
				zap.Int("changes", changed),
			)
		}
	}
}
