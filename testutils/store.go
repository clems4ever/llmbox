package testutils

import (
	"time"

	"github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// NoopStore is a store.Store that persists nothing: writes are dropped and reads
// find nothing. Tests that don't exercise persistence pass it to hub.New.
type NoopStore struct{}

// PutBox discards the box.
//
// @arg _ The box to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutBox(_ store.Box) error { return nil }

// DeleteBox does nothing.
//
// @arg _ The token to (not) delete.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteBox(_ string) error { return nil }

// ListBoxes returns no boxes.
//
// @return []store.Box Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) ListBoxes() ([]store.Box, error) { return nil, nil }

// Close does nothing.
//
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) Close() error { return nil }

// PutOIDCFlow discards the flow.
//
// @arg _ The OAuth state key.
// @arg _ The flow to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutOIDCFlow(_ string, _ store.OIDCFlow) error { return nil }

// TakeOIDCFlow finds nothing.
//
// @arg _ The OAuth state key.
// @return store.OIDCFlow The zero flow.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) TakeOIDCFlow(_ string) (store.OIDCFlow, bool, error) {
	return store.OIDCFlow{}, false, nil
}

// PutIdentitySession discards the session.
//
// @arg _ The opaque session id.
// @arg _ The session to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutIdentitySession(_ string, _ store.IdentitySession) error { return nil }

// GetIdentitySession finds nothing.
//
// @arg _ The opaque session id.
// @return store.IdentitySession The zero session.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) GetIdentitySession(_ string) (store.IdentitySession, bool, error) {
	return store.IdentitySession{}, false, nil
}

// DeleteIdentitySession does nothing.
//
// @arg _ The opaque session id.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteIdentitySession(_ string) error { return nil }

// PurgeExpiredIdentities does nothing.
//
// @arg _ The cutoff time.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PurgeExpiredIdentities(_ time.Time) error { return nil }

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

// PutSetting discards the setting.
//
// @arg _ The setting key.
// @arg _ The value to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutSetting(_, _ string) error { return nil }

// GetSetting finds nothing.
//
// @arg _ The setting key.
// @return string The empty value.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) GetSetting(_ string) (string, bool, error) { return "", false, nil }

// PutAPIKey discards the API key.
//
// @arg _ The key's secret hash.
// @arg _ The record to (not) persist.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) PutAPIKey(_ string, _ store.APIKeyRecord) error { return nil }

// GetAPIKey finds nothing.
//
// @arg _ The key's secret hash.
// @return store.APIKeyRecord The zero record.
// @return bool Always false.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) GetAPIKey(_ string) (store.APIKeyRecord, bool, error) {
	return store.APIKeyRecord{}, false, nil
}

// ListAPIKeys returns no keys.
//
// @return []store.APIKeyInfo Always nil.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) ListAPIKeys() ([]store.APIKeyInfo, error) { return nil, nil }

// DeleteAPIKey does nothing.
//
// @arg _ The key's hash ID.
// @error error Always nil.
//
// @testcase TestNoopStore checks every no-op method is inert.
func (NoopStore) DeleteAPIKey(_ string) error { return nil }
