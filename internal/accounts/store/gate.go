package store

import "context"

// GateProviders wraps st so accounts of a switched-off provider read as
// StatusProviderDisabled. Tools and the send engine use the wrapped store, so
// those accounts are hidden from every read and write path and report
// provider_disabled in accounts[]; their credentials and every stored field
// are untouched, and turning the provider back on restores them. The connect
// page and token refresh use the unwrapped store and see the real status.
//
// enabled reports whether a provider name (ProviderGoogle, ProviderMicrosoft)
// is on. An empty provider means Google, as Account.upgrade reads it.
func GateProviders(st Store, enabled func(provider string) bool) Store {
	return &gated{Store: st, enabled: enabled}
}

type gated struct {
	Store
	enabled func(provider string) bool
}

func (g *gated) mark(a Account) Account {
	p := a.Provider
	if p == "" {
		p = ProviderGoogle
	}
	if !g.enabled(p) && a.Status != StatusDisconnected {
		a.Status = StatusProviderDisabled
	}
	return a
}

func (g *gated) GetAccount(ctx context.Context, owner, id string) (Account, error) {
	a, err := g.Store.GetAccount(ctx, owner, id)
	if err != nil {
		return a, err
	}
	return g.mark(a), nil
}

func (g *gated) ListAccounts(ctx context.Context, owner string) ([]Account, error) {
	accts, err := g.Store.ListAccounts(ctx, owner)
	if err != nil {
		return nil, err
	}
	for i := range accts {
		accts[i] = g.mark(accts[i])
	}
	return accts, nil
}

func (g *gated) FindAccountByAddress(ctx context.Context, owner, address string) (Account, error) {
	a, err := g.Store.FindAccountByAddress(ctx, owner, address)
	if err != nil {
		return a, err
	}
	return g.mark(a), nil
}
