package testutils

import (
	"time"

	"github.com/clems4ever/llmbox/internal/cluster"
	"github.com/clems4ever/llmbox/internal/store"
)

// NoopStore is a store.Store that persists nothing: writes are dropped and reads
// find nothing. Tests that don't exercise persistence pass it to server.New.
type NoopStore struct{}

// Save discards the session.
//
// @arg _ The session to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) Save(_ store.PersistedSession) error { return nil }

// Delete does nothing.
//
// @arg _ The token to (not) delete.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) Delete(_ string) error { return nil }

// LoadAll returns no sessions.
//
// @return []store.PersistedSession Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) LoadAll() ([]store.PersistedSession, error) { return nil, nil }

// Close does nothing.
//
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) Close() error { return nil }

// SaveLoginFlow discards the flow.
//
// @arg _ The OAuth state key.
// @arg _ The flow to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) SaveLoginFlow(_ string, _ store.LoginFlow) error { return nil }

// TakeLoginFlow finds nothing.
//
// @arg _ The OAuth state key.
// @return store.LoginFlow The zero flow.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) TakeLoginFlow(_ string) (store.LoginFlow, bool, error) {
	return store.LoginFlow{}, false, nil
}

// SaveLoginSession discards the session.
//
// @arg _ The opaque session id.
// @arg _ The session to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) SaveLoginSession(_ string, _ store.LoginSession) error { return nil }

// LoginSession finds nothing.
//
// @arg _ The opaque session id.
// @return store.LoginSession The zero session.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) LoginSession(_ string) (store.LoginSession, bool, error) {
	return store.LoginSession{}, false, nil
}

// DeleteLoginSession does nothing.
//
// @arg _ The opaque session id.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteLoginSession(_ string) error { return nil }

// PurgeExpiredLogins does nothing.
//
// @arg _ The cutoff time.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PurgeExpiredLogins(_ time.Time) error { return nil }

// PutJoinToken discards the token.
//
// @arg _ The token hash key.
// @arg _ The token record to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutJoinToken(_ string, _ cluster.JoinTokenRecord) error { return nil }

// TakeJoinToken finds nothing.
//
// @arg _ The token hash key.
// @return cluster.JoinTokenRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) TakeJoinToken(_ string) (cluster.JoinTokenRecord, bool, error) {
	return cluster.JoinTokenRecord{}, false, nil
}

// ListJoinTokens returns no tokens.
//
// @return []cluster.JoinTokenInfo Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) ListJoinTokens() ([]cluster.JoinTokenInfo, error) { return nil, nil }

// DeleteJoinToken does nothing.
//
// @arg _ The join token hash ID.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteJoinToken(_ string) error { return nil }

// PutSpoke discards the spoke.
//
// @arg _ The spoke name key.
// @arg _ The spoke record to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutSpoke(_ string, _ cluster.SpokeRecord) error { return nil }

// GetSpoke finds nothing.
//
// @arg _ The spoke name key.
// @return cluster.SpokeRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) GetSpoke(_ string) (cluster.SpokeRecord, bool, error) {
	return cluster.SpokeRecord{}, false, nil
}

// ListSpokes returns no spokes.
//
// @return []cluster.SpokeRecord Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) ListSpokes() ([]cluster.SpokeRecord, error) { return nil, nil }

// DeleteSpoke does nothing.
//
// @arg _ The spoke name key.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteSpoke(_ string) error { return nil }

// SaveProxy discards the proxy.
//
// @arg _ The proxy record to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) SaveProxy(_ store.ProxyRecord) error { return nil }

// GetProxy finds nothing.
//
// @arg _ The proxy slug key.
// @return store.ProxyRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) GetProxy(_ string) (store.ProxyRecord, bool, error) {
	return store.ProxyRecord{}, false, nil
}

// ListProxies returns no proxies.
//
// @return []store.ProxyRecord Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) ListProxies() ([]store.ProxyRecord, error) { return nil, nil }

// DeleteProxy does nothing.
//
// @arg _ The proxy slug key.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteProxy(_ string) error { return nil }
