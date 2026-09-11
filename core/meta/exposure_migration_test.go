package meta

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/autodb/core/config"
)

func TestMigrateV16_BackfillsEffectiveExposurePerRow(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  func(*testing.T) config.Meta
	}{
		{"sqlite", func(t *testing.T) config.Meta {
			return config.Meta{Engine: "sqlite", Path: filepath.Join(t.TempDir(), "meta.db")}
		}},
		{"postgres", func(t *testing.T) config.Meta {
			return config.Meta{Engine: "postgres", DSN: scratchDSN(t), AllowInsecureDSN: true}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)
			before := openBeforeExposureMigration(t, cfg)
			rootID := addUser(t, before, "root")

			want := map[string]int64{
				"legacy-closed":  0,
				"legacy-session": 1,
			}
			for name, exposed := range want {
				profile := ProfileV1Compat
				if exposed != 0 {
					profile = ProfileSession
				}
				if _, err := before.Connections.OnCtx(ctx).
					Set(ConnName, name).Set(ConnEngine, "sqlite").Set(ConnDSNEnc, []byte("enc")).
					Set(ConnProfile, profile).Set(ConnCreatedBy, rootID).
					Set(ConnCreatedAt, int64(1)).Set(ConnUpdatedAt, int64(1)).Insert(); err != nil {
					t.Fatalf("seeding %s: %v", name, err)
				}
			}
			if err := before.Close(); err != nil {
				t.Fatalf("closing pre-v16 store: %v", err)
			}

			after, err := Open(ctx, cfg)
			if err != nil {
				t.Fatalf("upgrading through v16: %v", err)
			}
			t.Cleanup(func() { _ = after.Close() })

			for name, exposed := range want {
				row, err := after.Connections.OnCtx(ctx).With(ConnName, name).Get()
				if err != nil {
					t.Fatalf("reading %s after upgrade: %v", name, err)
				}
				if row.FrontDoorExposed != exposed {
					t.Errorf("%s backfilled frontdoor_exposed=%d, want %d", name, row.FrontDoorExposed, exposed)
				}
			}

			id, err := after.Connections.OnCtx(ctx).
				Set(ConnName, "new-default").Set(ConnEngine, "sqlite").Set(ConnDSNEnc, []byte("enc")).
				Set(ConnCreatedBy, rootID).Set(ConnCreatedAt, int64(2)).Set(ConnUpdatedAt, int64(2)).Insert()
			if err != nil {
				t.Fatalf("inserting a post-v16 connection: %v", err)
			}
			row, err := after.Connections.OnCtx(ctx).With(ConnID, id).Get()
			if err != nil {
				t.Fatal(err)
			}
			if row.FrontDoorExposed != 0 {
				t.Fatalf("new connection defaulted frontdoor_exposed=%d, want 0", row.FrontDoorExposed)
			}
		})
	}
}

func openBeforeExposureMigration(t *testing.T, cfg config.Meta) *Store {
	t.Helper()
	full := migrations
	capped := make([]migration, 0, len(full))
	for _, m := range full {
		if m.Version < 16 {
			capped = append(capped, m)
		}
	}
	migrations = capped
	defer func() { migrations = full }()

	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening pre-v16 store: %v", err)
	}
	return s
}
