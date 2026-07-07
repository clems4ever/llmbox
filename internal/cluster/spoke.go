package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// ErrEnrollRejected reports that the hub refused enrollment (bad/expired/used
// join token, or a credential that no longer matches). It is terminal: retrying
// with the same token will not succeed, so a caller should stop rather than
// reconnect.
var ErrEnrollRejected = errors.New("enrollment rejected by hub")

// Credentials is a spoke's persisted enrollment state: the name the hub assigned
// and the bearer credential it minted. A spoke saves this after first
// enrollment and presents it to reconnect without the (one-time) join token.
type Credentials struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// Dialer establishes a transport to the hub. It exists so Run can be tested over
// an in-memory transport instead of a real WebSocket.
type Dialer func(ctx context.Context) (transport, error)

// WebSocketDialer dials the hub's spoke-connect URL over a WebSocket.
//
// SECURITY — transport confidentiality and integrity are the DEPLOYMENT's
// responsibility, not this code's. The enrollment handshake sends the one-time
// join token and then the long-lived bearer credential in the first frames, and
// every box verb (including the user's code and session details) flows over this
// socket. Use wss:// in production and terminate TLS at a trusted reverse proxy
// in front of the hub; a ws:// URL sends the credential and all traffic in
// cleartext and MUST only be used on a fully trusted network (e.g. loopback or a
// private mesh). This dialer accepts whichever scheme the operator passes — it
// does not (and cannot) verify the link is encrypted. The bearer credential is a
// static secret presented verbatim on every reconnect, so anyone who captures it
// (on the wire or at rest) can impersonate this spoke until it is revoked. For a
// wss:// hub with a private-CA or self-signed certificate, use WebSocketDialerTLS
// to trust the right CA (preferred) rather than disabling verification.
//
// @arg url The hub's spoke-connect URL (ws:// or wss://).
// @return Dialer A dialer that opens a WebSocket transport to that URL.
//
// @testcase TestSpokeRunEnrollsAndServes uses an in-memory dialer in place of this.
func WebSocketDialer(url string) Dialer {
	return WebSocketDialerTLS(url, nil)
}

// WebSocketDialerTLS is WebSocketDialer with a caller-supplied TLS client config
// for wss:// links — used to trust a hub's private-CA / self-signed certificate
// (config.RootCAs) or, as a last resort for testing, to skip verification
// (config.InsecureSkipVerify). A nil config uses the system trust store, exactly
// like WebSocketDialer. The same SECURITY notes as WebSocketDialer apply.
//
// @arg url The hub's spoke-connect URL (ws:// or wss://).
// @arg tlsConf The TLS client config for wss:// dials; nil uses the system default.
// @return Dialer A dialer that opens a WebSocket transport to that URL.
//
// @testcase TestHubEnrollAndRoute dials a real hub through this (with a nil config).
func WebSocketDialerTLS(url string, tlsConf *tls.Config) Dialer {
	return func(ctx context.Context) (transport, error) {
		var opts *websocket.DialOptions
		if tlsConf != nil {
			opts = &websocket.DialOptions{
				HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf.Clone()}},
			}
		}
		conn, _, err := websocket.Dial(ctx, url, opts)
		if err != nil {
			return nil, err
		}
		return newWSTransport(conn), nil
	}
}

// Run connects a spoke to the hub and serves box verbs against mgr until ctx is
// cancelled or the connection drops. It enrolls using joinToken when creds is
// nil; otherwise it reconnects with the saved creds. On first enrollment it
// invokes save with the minted credentials so the caller can persist them. Run
// returns when the connection ends; the caller decides whether to retry.
//
// @arg ctx Context whose cancellation stops the spoke.
// @arg dial The dialer establishing the transport to the hub.
// @arg mgr The local box manager verbs are executed against.
// @arg joinToken The one-time join token for first enrollment; ignored when creds is set.
// @arg creds Saved credentials for reconnect; nil for first enrollment.
// @arg save Callback invoked with freshly minted credentials on first enrollment; may be nil.
// @arg policy The admission policy the spoke applies to box-creation requests.
// @error error if dialing, enrollment, or the serve loop fails.
//
// @testcase TestSpokeRunEnrollsAndServes enrolls with a join token and serves a verb.
// @testcase TestSpokeRunReconnectsWithCreds reconnects using saved credentials.
// @testcase TestSpokeRunEnrollRejected returns the hub's rejection error.
func Run(ctx context.Context, dial Dialer, mgr BoxManager, joinToken string, creds *Credentials, save func(Credentials) error, policy ValidationPolicy) error {
	tr, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("dialing hub: %w", err)
	}
	defer func() { _ = tr.Close() }()

	got, err := enrollSpoke(ctx, tr, joinToken, creds)
	if err != nil {
		return err
	}
	if creds == nil && save != nil {
		if err := save(got); err != nil {
			return fmt.Errorf("saving spoke credentials: %w", err)
		}
	}

	return serve(ctx, tr, mgr, policy)
}

// enrollSpoke performs the spoke's side of the enrollment handshake and returns
// the resulting credentials (name plus, on first enrollment, the minted
// credential to save).
//
// @arg ctx Context bounding the handshake.
// @arg tr The transport to the hub.
// @arg joinToken The one-time join token for first enrollment; ignored when creds is set.
// @arg creds Saved credentials for reconnect; nil for first enrollment.
// @return Credentials The name and (on first enrollment) minted credential.
// @error error if the handshake fails or the hub rejects enrollment.
//
// @testcase TestSpokeRunEnrollsAndServes covers the first-enrollment handshake.
// @testcase TestSpokeRunReconnectsWithCreds covers the reconnect handshake.
// @testcase TestSpokeRunEnrollRejected surfaces the hub's rejection.
func enrollSpoke(ctx context.Context, tr transport, joinToken string, creds *Credentials) (Credentials, error) {
	var req enrollReq
	if creds != nil {
		req = enrollReq{Name: creds.Name, Credential: creds.Credential}
	} else {
		req = enrollReq{JoinToken: joinToken}
	}
	payload, err := encodePayload(req)
	if err != nil {
		return Credentials{}, err
	}
	if err := tr.Send(ctx, frame{Type: frameEnroll, Payload: payload}); err != nil {
		return Credentials{}, fmt.Errorf("sending enrollment: %w", err)
	}

	f, err := tr.Recv(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("awaiting enrollment reply: %w", err)
	}
	if f.Type == frameErr {
		return Credentials{}, fmt.Errorf("%w: %s", ErrEnrollRejected, f.Error)
	}
	if f.Type != frameWelcome {
		return Credentials{}, fmt.Errorf("unexpected enrollment reply %q", f.Type)
	}
	var welcome welcomeResp
	if err := decodePayload(f.Payload, &welcome); err != nil {
		return Credentials{}, err
	}
	out := Credentials{Name: welcome.Name, Credential: welcome.Credential}
	if creds != nil {
		// Reconnect: keep using the saved credential the hub validated.
		out.Credential = creds.Credential
	}
	return out, nil
}
