package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/yongjohnlee80/golib/dao"

	"github.com/yongjohnlee80/autodb/core/config"
)

func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), config.Meta{Engine: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// addUser inserts a minimal valid user and returns its id.
func addUser(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	id, err := s.Users.OnCtx(context.Background()).
		Set(UserName, name).Set(UserRole, RoleAdmin).
		Set(UserPassHash, []byte{1, 2, 3}).Set(UserDisabled, int64(0)).
		Set(UserCreatedAt, int64(100)).Set(UserUpdatedAt, int64(100)).
		Insert()
	if err != nil {
		t.Fatalf("insert user %s: %v", name, err)
	}
	return id
}

func TestOpen_MigratesAndRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMem(t)

	id := addUser(t, s, "root")
	if id <= 0 {
		t.Fatalf("user id = %d, want > 0", id)
	}
	got, err := s.Users.OnCtx(ctx).With(UserID, id).Get()
	if err != nil || got.Name != "root" || got.Role != RoleAdmin {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

func TestOpen_ConstraintsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMem(t)
	addUser(t, s, "root")

	// Unique name.
	_, err := s.Users.OnCtx(ctx).
		Set(UserName, "root").Set(UserRole, RoleReader).
		Set(UserPassHash, []byte{9}).Set(UserDisabled, int64(0)).
		Set(UserCreatedAt, int64(1)).Set(UserUpdatedAt, int64(1)).
		Insert()
	if !errors.Is(err, dao.ErrDuplicate) {
		t.Errorf("dup user err = %v, want ErrDuplicate", err)
	}

	// Foreign key: connection with a nonexistent creator.
	_, err = s.Connections.OnCtx(ctx).
		Set(ConnName, "gold").Set(ConnEngine, "postgres").
		Set(ConnDSNEnc, []byte{1}).Set(ConnCreatedBy, int64(999)).
		Set(ConnCreatedAt, int64(1)).Set(ConnUpdatedAt, int64(1)).
		Insert()
	if !errors.Is(err, dao.ErrForeignKey) {
		t.Errorf("bad created_by err = %v, want ErrForeignKey", err)
	}
}

func TestStoreMeta_KV(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openMem(t)

	if _, ok, err := s.GetMeta(ctx, "missing"); err != nil || ok {
		t.Fatalf("GetMeta(missing) = ok=%v err=%v, want absent", ok, err)
	}
	if err := s.SetMeta(ctx, "install_id", "abc"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := s.SetMeta(ctx, "install_id", "xyz"); err != nil {
		t.Fatalf("SetMeta upsert: %v", err)
	}
	v, ok, err := s.GetMeta(ctx, "install_id")
	if err != nil || !ok || v != "xyz" {
		t.Fatalf("GetMeta = %q ok=%v err=%v, want xyz", v, ok, err)
	}
}

func TestOpen_ReopenIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.db")

	s1, err := Open(ctx, config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	addUser(t, s1, "root")
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(ctx, config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	n, err := s2.Users.OnCtx(ctx).Count()
	if err != nil || n != 1 {
		t.Fatalf("Count after reopen = %d, %v", n, err)
	}
}

func TestOpen_DowngradeGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.db")

	s1, err := Open(ctx, config.Meta{Engine: "sqlite", Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Simulate a store written by a newer binary.
	if _, err := s1.Conn().ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (999, 0)`); err != nil {
		t.Fatalf("seeding future version: %v", err)
	}
	_ = s1.Close()

	if _, err := Open(ctx, config.Meta{Engine: "sqlite", Path: path}); err == nil {
		t.Fatal("Open succeeded against a newer-schema store, want downgrade-guard error")
	}
}

func TestMigrate_RefusesWrongDirections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := openMem(t), openMem(t)

	// sqlite → sqlite is not an engine migration.
	if err := MigrateToPostgres(ctx, a, b); !errors.Is(err, ErrMigrate) {
		t.Errorf("sqlite→sqlite err = %v, want ErrMigrate", err)
	}
}
