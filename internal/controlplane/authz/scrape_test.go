package authz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
)

// seededRanks are what migration 000001 puts in the `roles` table. The guard reads them from there
// at startup rather than from Go constants, so a test that hard-coded a different ordering would be
// testing something the product does not do.
func testGuard() *Guard {
	return &Guard{
		ranks: map[string]int{RoleViewer: 10, RoleOperator: 20, RoleDBA: 30, RoleAdmin: 40},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAuthorizeScrape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal authn.Principal
		anonymous bool
		want      error
	}{
		{
			name:      "nobody",
			anonymous: true,
			want:      ErrScrapeNoCredential,
		},
		{
			// Trap 5 of the B8 brief, and the reason this test exists at all. The bootstrap
			// credential holds *no grants* — Check allows it by Kind — so a rule written as
			// "does this caller hold a tenant-wide viewer grant" refuses it. It is the credential
			// the development stack's own scrape config presents, so the mistake would show up as a
			// metrics store that silently collects nothing rather than as a failing test.
			name:      "the bootstrap credential, which holds no grants",
			principal: authn.Principal{Kind: authn.KindBootstrap, Actor: "bootstrap", TenantID: "t"},
			want:      nil,
		},
		{
			// The same shape: constructed in-process, no credential, no grants. Nothing that parses
			// an HTTP request can produce one, so allowing it by kind grants nothing to a stranger.
			name:      "a system actor",
			principal: authn.System("scheduler", "t"),
			want:      nil,
		},
		{
			name: "a tenant-wide viewer",
			principal: authn.Principal{
				Kind: authn.KindUser, TenantID: "t",
				Grants: []authn.Grant{{Role: RoleViewer, Rank: 10}},
			},
			want: nil,
		},
		{
			name: "a tenant-wide admin, since grants are additive and the highest rank wins",
			principal: authn.Principal{
				Kind: authn.KindUser, TenantID: "t",
				Grants: []authn.Grant{{Role: RoleAdmin, Rank: 40}},
			},
			want: nil,
		},
		{
			// A scrape names no scope, and a request that names no scope is a question about the
			// whole tenant (ADR-0035). The response carries a series per instance, so answering
			// this caller would hand them the shape of an estate they were granted three servers of.
			name: "a viewer on one instance",
			principal: authn.Principal{
				Kind: authn.KindUser, TenantID: "t",
				Grants: []authn.Grant{{Role: RoleViewer, Rank: 10, InstanceID: "instance-1"}},
			},
			want: ErrScrapeForbidden,
		},
		{
			// Even a dba, if the grant is on an environment rather than the tenant. The rank is not
			// the question; the scope is.
			name: "a dba on one environment",
			principal: authn.Principal{
				Kind: authn.KindUser, TenantID: "t",
				Grants: []authn.Grant{{Role: RoleDBA, Rank: 30, EnvironmentID: "production"}},
			},
			want: ErrScrapeForbidden,
		},
		{
			name:      "an authenticated caller holding nothing at all",
			principal: authn.Principal{Kind: authn.KindUser, TenantID: "t"},
			want:      ErrScrapeForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.anonymous {
				ctx = authn.WithPrincipal(ctx, tc.principal)
			} else {
				ctx = authn.WithPrincipal(ctx, authn.Anonymous())
			}

			err := testGuard().AuthorizeScrape(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestAuthorizeScrapeRefusesWithNoPrincipalOnTheContext covers the case that is not a caller at all:
// a context nothing put a principal on. Refusing rather than allowing is the only safe reading of
// "I do not know who this is".
func TestAuthorizeScrapeRefusesWithNoPrincipalOnTheContext(t *testing.T) {
	if err := testGuard().AuthorizeScrape(context.Background()); !errors.Is(err, ErrScrapeNoCredential) {
		t.Fatalf("got %v, want %v", err, ErrScrapeNoCredential)
	}
}

// TestAuthorizeScrapeRefusesWhenTheRanksAreMissing is the same reasoning one level down. NewGuard
// fails without the seeded ranks so this cannot happen in a control plane that started, and if it
// somehow did, "I do not know what viewer means" must not mean "everyone".
func TestAuthorizeScrapeRefusesWhenTheRanksAreMissing(t *testing.T) {
	g := &Guard{ranks: map[string]int{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := authn.WithPrincipal(context.Background(), authn.Principal{
		Kind: authn.KindUser, TenantID: "t",
		Grants: []authn.Grant{{Role: RoleAdmin, Rank: 40}},
	})

	if err := g.AuthorizeScrape(ctx); !errors.Is(err, ErrScrapeForbidden) {
		t.Fatalf("got %v, want %v", err, ErrScrapeForbidden)
	}
}
