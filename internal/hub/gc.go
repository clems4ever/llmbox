package hub

import (
	"fmt"

	"github.com/clems4ever/llmbox/internal/hub/hooks"
)

// knownSpokeNames returns the set of spoke names that still exist: every spoke
// currently enrolled in the cluster store. A spoke that has been de-enrolled
// (removed) is absent — objects pinned to it have "departed". Enrollment, not live
// connectivity, is the test: a spoke that is merely offline is still known and its
// boxes are kept, because it may reconnect.
//
// @return map[string]bool The set of still-existing spoke names.
// @error error if the enrolled spokes cannot be read from the store.
//
// @testcase TestPruneDepartedSpokesRemovesStaleObjects keeps enrolled spokes.
func (s *Server) knownSpokeNames() (map[string]bool, error) {
	known := map[string]bool{}
	enrolled, err := s.store.ListSpokes()
	if err != nil {
		return nil, err
	}
	for _, rec := range enrolled {
		known[rec.Name] = true
	}
	return known, nil
}

// PruneDepartedSpokes removes every session and proxy pinned to a spoke that no
// longer exists — a spoke de-enrolled from the cluster. Any still-enrolled spoke
// (even if momentarily disconnected) is kept, so a spoke that is merely offline is
// never purged; only one that has truly disappeared. Purged sessions have their
// destroy hooks replayed, exactly like a reap. It returns the box IDs of the
// purged sessions.
//
// This is what keeps box-ID resolution unambiguous over time: a box whose spoke
// was removed can no longer linger as a duplicate session and be selected at
// random. Call it at startup (after Restore) and periodically from ReapLoop.
//
// @return []string The box IDs of the sessions that were purged.
// @error error if the enrolled spokes cannot be read from the store.
//
// @testcase TestPruneDepartedSpokesRemovesStaleObjects purges a de-enrolled spoke's session and proxy.
// @testcase TestPruneDepartedSpokesKeepsOfflineEnrolledSpoke keeps an enrolled-but-offline spoke's objects.
func (s *Server) PruneDepartedSpokes() ([]string, error) {
	// Departed-spoke GC is a clustering concern: only a hub authoritatively knows
	// the set of enrolled spokes. With clustering disabled there is no such
	// authority, so we never conclude a spoke has departed and never purge — a
	// remote-spoke session simply belongs to a cluster not active right now.
	if s.hub == nil {
		return nil, nil
	}
	known, err := s.knownSpokeNames()
	if err != nil {
		return nil, fmt.Errorf("listing enrolled spokes: %w", err)
	}

	s.mu.Lock()
	var purgedBoxIDs, droppedTokens []string
	var torn []tornBox
	for tok, sess := range s.byToken {
		if known[s.resolveStoredSpoke(sess.SpokeName)] {
			continue
		}
		delete(s.byToken, tok)
		droppedTokens = append(droppedTokens, tok)
		torn = append(torn, tornBox{boxID: sess.BoxID, state: sess.HookState})
		if sess.BoxID != "" {
			purgedBoxIDs = append(purgedBoxIDs, sess.BoxID)
		}
	}
	s.mu.Unlock()

	for _, tok := range droppedTokens {
		// The token is a secret (it forms the auth URL), so it is never logged.
		if err := s.store.Delete(tok); err != nil {
			s.logger().Warn("failed to delete session for departed spoke", "err", err)
		}
	}
	for _, tb := range torn {
		s.runDestroyHooks(hooks.BoxInfo{BoxID: tb.boxID}, tb.state)
	}

	// Drop proxies pinned to a departed spoke directly (by spoke, not by box ID):
	// this also reaps a proxy whose session was already gone, and never touches a
	// proxy for a same-box-ID box living on a still-known spoke.
	s.pruneProxiesForDepartedSpokes(known)
	return purgedBoxIDs, nil
}

// pruneProxiesForDepartedSpokes deletes every enabled proxy whose spoke is not in
// known (the enrolled spokes). It is best-effort: a delete failure is logged and
// the rest proceed. No-op when proxying is disabled.
//
// @arg known The set of still-existing spoke names.
//
// @testcase TestPruneDepartedSpokesRemovesStaleObjects deletes a departed spoke's proxy.
func (s *Server) pruneProxiesForDepartedSpokes(known map[string]bool) {
	if !s.ProxyEnabled() {
		return
	}
	proxies, err := s.store.ListProxies()
	if err != nil {
		s.logger().Warn("listing proxies to prune departed spokes", "err", err)
		return
	}
	for _, p := range proxies {
		if known[s.resolveStoredSpoke(p.Spoke)] {
			continue
		}
		if err := s.store.DeleteProxy(p.Slug); err != nil {
			s.logger().Warn("deleting proxy for departed spoke", "slug", p.Slug, "err", err)
		}
	}
}
