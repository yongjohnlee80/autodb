package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	golibrpc "github.com/yongjohnlee80/golib/server/rpc"

	"github.com/yongjohnlee80/autodb/core/notes"
)

// The notes verbs: a caller's own notes, served from the one notes store
// (core/notes) rooted at the server's notes directory, on both surfaces. A
// remote session reads and writes the server's notes, never the client
// machine's.
//
// Authorization follows ownership. The store is the caller's own root,
// derived from the canonical name its token resolves to, never from a
// parameter, so no workspace id can reach anyone else's notes. A folder can
// outlive its workspace ("detached"); reading, writing and deleting an
// existing note in it stays the owner's. Creating a note, or writing a new
// one, needs a workspace the caller can see now: otherwise a caller could
// make folders for any id.
//
// A note's version is the SHA-256 of its content, hex, or "" for a note that
// does not exist.

// noteStores holds one store per subject, opened on first use.
type noteStores struct {
	mu     sync.Mutex
	stores map[string]*notes.Store
}

// notesFor is the caller's own store.
func (s *Server) notesFor(subject string) (*notes.Store, error) {
	if s.notesDir == "" {
		return nil, &golibrpc.Error{Code: CodeNotesUnavailable, Message: "this server keeps no notes"}
	}
	s.noteStores.mu.Lock()
	defer s.noteStores.mu.Unlock()
	if st, ok := s.noteStores.stores[subject]; ok {
		return st, nil
	}
	st, err := notes.NewPersonalNotes(s.notesDir, subject)
	if err != nil {
		return nil, err
	}
	if s.noteStores.stores == nil {
		s.noteStores.stores = map[string]*notes.Store{}
	}
	s.noteStores.stores[subject] = st
	return st, nil
}

func noteVersion(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// noteErr maps the store's refusals onto the wire.
func (s *Server) noteErr(req *golibrpc.Request, err error) error {
	var re *golibrpc.Error
	switch {
	case errors.As(err, &re):
		return re
	case errors.Is(err, notes.ErrNoteConflict):
		return &golibrpc.Error{Code: CodeNoteConflict, Message: "the note changed since it was read"}
	case errors.Is(err, notes.ErrBadWorkspace):
		return &golibrpc.Error{Code: golibrpc.CodeInvalidParams, Message: "not a workspace id"}
	case errors.Is(err, notes.ErrRemovedNotDurable):
		return &golibrpc.Error{Code: CodeNoteRemovalUncertain,
			Message: "the note was removed, but the removal may not survive a crash; do not retry"}
	}
	return s.wireErrFor(req, err)
}

// noteArgs reads (token, ws[, name]) and resolves the caller's store.
func (s *Server) noteArgs(ctx context.Context, req *golibrpc.Request, withName bool) (store *notes.Store, token string, ws int64, name string, err error) {
	token, err = argStr(req.Params, 0, "token")
	if err != nil {
		return
	}
	if ws, err = argInt(req.Params, 1, "workspace_id"); err != nil {
		return
	}
	if withName {
		if name, err = argStr(req.Params, 2, "name"); err != nil {
			return
		}
		if name, err = notes.CleanName(name); err != nil {
			err = &golibrpc.Error{Code: golibrpc.CodeInvalidParams, Message: err.Error()}
			return
		}
	}
	ident, verr := s.auth.ValidateToken(ctx, token)
	if verr != nil {
		err = s.wireErrFor(req, verr)
		return
	}
	store, err = s.notesFor(ident.Name())
	return
}

// visibleWorkspace refuses a workspace the caller cannot see now.
func (s *Server) visibleWorkspace(ctx context.Context, req *golibrpc.Request, token string, ws int64) error {
	views, err := s.eng.ListWorkspaces(ctx, token)
	if err != nil {
		return s.wireErrFor(req, err)
	}
	for _, v := range views {
		if v.ID == ws {
			return nil
		}
	}
	return &golibrpc.Error{Code: CodeWorkspaceNotVisible, Message: "that workspace is not one you can see"}
}

func (s *Server) registerNotes() {
	// notes.workspaces(token) → [{id, name, detached}]: every folder in the
	// caller's notes, named when its workspace is one the caller can see,
	// detached otherwise.
	s.handle("notes.workspaces", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 1); err != nil {
			return nil, err
		}
		token, err := argStr(req.Params, 0, "token")
		if err != nil {
			return nil, err
		}
		ident, err := s.auth.ValidateToken(ctx, token)
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		store, err := s.notesFor(ident.Name())
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		dirs, err := store.ListWorkspaceDirs()
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		views, err := s.eng.ListWorkspaces(ctx, token)
		if err != nil {
			return nil, s.wireErrFor(req, err)
		}
		names := map[int64]string{}
		for _, v := range views {
			names[v.ID] = v.Name
		}
		out := make([]any, 0, len(dirs))
		for _, id := range dirs {
			name, seen := names[id]
			out = append(out, map[string]any{"id": id, "name": name, "detached": !seen})
		}
		return out, nil
	})
	s.handle("notes.list", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 2); err != nil {
			return nil, err
		}
		store, _, ws, _, err := s.noteArgs(ctx, req, false)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		names, err := store.List(ws)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, n)
		}
		return out, nil
	})
	// notes.read(token, ws, name) → {body, version}; a note that does not
	// exist reads as "" at version "".
	s.handle("notes.read", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		store, _, ws, name, err := s.noteArgs(ctx, req, true)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		n, body, err := store.Load(ws, name)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		version := ""
		if n.Existed() {
			version = noteVersion(body)
		}
		return map[string]any{"body": body, "version": version}, nil
	})
	// notes.create(token, ws, name) → {version}: an empty note, in a
	// workspace the caller can see. A name in use is a conflict.
	s.handle("notes.create", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		store, token, ws, name, err := s.noteArgs(ctx, req, true)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		if err := s.visibleWorkspace(ctx, req, token, ws); err != nil {
			return nil, err
		}
		if n, _, lerr := store.Load(ws, name); lerr == nil && n.Existed() {
			return nil, s.noteErr(req, notes.ErrNoteConflict)
		}
		if _, err := store.Create(ws, name); err != nil {
			return nil, s.noteErr(req, err)
		}
		return map[string]any{"version": noteVersion("")}, nil
	})
	// notes.write(token, ws, name, body, expected_version) → {version}. The
	// note must still be at expected_version ("" for one that must not exist
	// yet), checked against the server's own reading of it and again at the
	// replacement; otherwise it is a conflict. A new note needs a workspace
	// the caller can see.
	s.handle("notes.write", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 5); err != nil {
			return nil, err
		}
		store, token, ws, name, err := s.noteArgs(ctx, req, true)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		body, err := argStr(req.Params, 3, "body")
		if err != nil {
			return nil, err
		}
		expected, err := argStr(req.Params, 4, "expected_version")
		if err != nil {
			return nil, err
		}
		if expected == "" {
			if err := s.visibleWorkspace(ctx, req, token, ws); err != nil {
				return nil, err
			}
		}
		n, cur, err := store.Load(ws, name)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		at := ""
		if n.Existed() {
			at = noteVersion(cur)
		}
		if at != expected {
			return nil, s.noteErr(req, notes.ErrNoteConflict)
		}
		if err := store.Save(n, body); err != nil {
			return nil, s.noteErr(req, err)
		}
		return map[string]any{"version": noteVersion(body)}, nil
	})
	s.handle("notes.delete", func(ctx context.Context, req *golibrpc.Request) (any, error) {
		if err := exactArgs(req.Params, 3); err != nil {
			return nil, err
		}
		store, _, ws, name, err := s.noteArgs(ctx, req, true)
		if err != nil {
			return nil, s.noteErr(req, err)
		}
		return nil, s.noteErr(req, store.Delete(ws, name))
	})
}
