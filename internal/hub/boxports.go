package hub

import (
	"context"
	"fmt"

	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// This file implements cluster.BoxPortService: the hub-side enforcement of
// box-originated port requests. A request arrives over a spoke's authenticated
// cluster connection carrying only the box ID that SPOKE stamped (the box
// itself supplies just a port and description), and the hub independently
// verifies that box actually lives on that spoke before touching any proxy
// state. Identity is therefore never taken from anything running inside a
// sandbox — the spoke derives it from which per-box channel the request
// arrived on, and the hub re-checks the spoke's claim against its own records.

// boxPortSession resolves and authorizes a box-originated port request: the box
// ID must map to a known session AND that session must live on the
// authenticated spoke the request arrived on. Unknown box and wrong spoke
// return the same deliberately vague error, so a spoke cannot probe whether a
// box ID exists on another spoke.
//
// @arg spokeName The authenticated name of the spoke connection the request arrived on.
// @arg boxID The spoke-stamped box ID the request claims to originate from.
// @return *session The authorized session.
// @error error if the box ID is empty, unknown, or not owned by that spoke.
//
// @testcase TestOpenBoxPortWrongSpoke rejects a box owned by a different spoke.
// @testcase TestOpenBoxPortUnknownBox rejects an unknown box ID.
// @testcase TestOpenBoxPortEmptyBoxID rejects a box created without a box ID.
func (s *Server) boxPortSession(spokeName, boxID string) (*session, error) {
	if boxID == "" {
		return nil, fmt.Errorf("this box has no box ID, so it cannot publish ports; recreate it with a box ID")
	}
	sess := s.lookupByBoxID(boxID)
	if sess == nil || s.resolveStoredSpoke(sess.SpokeName) != spokeName {
		return nil, fmt.Errorf("no box %q found on spoke %q", boxID, spokeName)
	}
	return sess, nil
}

// boxPortInfo flattens a proxy record into the box-facing view: port, public
// URL, and description only — no slug, spoke, or creator.
//
// @arg rec The proxy record to flatten.
// @return cluster.BoxPortInfo The box-facing view of the record.
//
// @testcase TestOpenBoxPortCreatesProxy checks the returned view carries the proxy URL.
func (s *Server) boxPortInfo(rec store.ProxyRecord) cluster.BoxPortInfo {
	return cluster.BoxPortInfo{Port: rec.Port, URL: s.proxyURL(rec.Slug), Description: rec.Description}
}

// OpenBoxPort publishes a port of the box a spoke-originated request came from,
// implementing cluster.BoxPortService. It reuses createProxy, so it inherits
// its idempotency, port validation, and stale-container replacement; the proxy
// is recorded as created by "box:<box id>".
//
// @arg ctx Context for the request (unused; proxy state is hub-local).
// @arg spokeName The authenticated name of the spoke connection the request arrived on.
// @arg boxID The spoke-stamped box ID the request originates from.
// @arg port The TCP port inside the box to publish.
// @arg description An optional human-readable note for the port.
// @return cluster.BoxPortInfo The published port with its public URL.
// @error error if port publishing is disabled, the box is unknown or on another spoke, the port is invalid, or persistence fails.
//
// @testcase TestOpenBoxPortCreatesProxy publishes a port and stamps the box as creator.
// @testcase TestOpenBoxPortWrongSpoke rejects a box owned by a different spoke.
// @testcase TestOpenBoxPortProxyDisabled errors clearly when no proxy base domain is set.
func (s *Server) OpenBoxPort(ctx context.Context, spokeName, boxID string, port int, description string) (cluster.BoxPortInfo, error) {
	if !s.ProxyEnabled() {
		return cluster.BoxPortInfo{}, fmt.Errorf("port publishing is disabled on this hub: no proxy base domain is configured")
	}
	sess, err := s.boxPortSession(spokeName, boxID)
	if err != nil {
		return cluster.BoxPortInfo{}, err
	}
	rec, err := s.createProxy(sess.BoxID, port, "box:"+sess.BoxID, description)
	if err != nil {
		return cluster.BoxPortInfo{}, err
	}
	return s.boxPortInfo(rec), nil
}

// CloseBoxPort unpublishes a port of the box a spoke-originated request came
// from, implementing cluster.BoxPortService. It works even when the proxy base
// domain has since been unset, so a box can always clean up after itself.
//
// @arg ctx Context for the request (unused; proxy state is hub-local).
// @arg spokeName The authenticated name of the spoke connection the request arrived on.
// @arg boxID The spoke-stamped box ID the request originates from.
// @arg port The published port to close.
// @error error if the box is unknown or on another spoke, or no proxy exists for that port.
//
// @testcase TestCloseBoxPortDeletesProxy closes a previously published port.
// @testcase TestCloseBoxPortWrongSpoke rejects a box owned by a different spoke.
func (s *Server) CloseBoxPort(ctx context.Context, spokeName, boxID string, port int) error {
	sess, err := s.boxPortSession(spokeName, boxID)
	if err != nil {
		return err
	}
	_, err = s.deleteProxy(sess.BoxID, port)
	return err
}

// ListBoxPorts returns the published ports of the box a spoke-originated
// request came from — and only that box's — implementing
// cluster.BoxPortService.
//
// @arg ctx Context for the request (unused; proxy state is hub-local).
// @arg spokeName The authenticated name of the spoke connection the request arrived on.
// @arg boxID The spoke-stamped box ID the request originates from.
// @return []cluster.BoxPortInfo The box's published ports.
// @error error if the box is unknown or on another spoke, or the proxy list cannot be read.
//
// @testcase TestListBoxPortsScopedToBox lists only the requesting box's ports.
func (s *Server) ListBoxPorts(ctx context.Context, spokeName, boxID string) ([]cluster.BoxPortInfo, error) {
	sess, err := s.boxPortSession(spokeName, boxID)
	if err != nil {
		return nil, err
	}
	recs, err := s.listProxies(sess.BoxID)
	if err != nil {
		return nil, err
	}
	out := make([]cluster.BoxPortInfo, 0, len(recs))
	for _, rec := range recs {
		out = append(out, s.boxPortInfo(rec))
	}
	return out, nil
}
