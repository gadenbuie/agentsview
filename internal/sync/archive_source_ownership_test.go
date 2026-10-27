package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAttachedClaudeDAGCollisionPreservesExistingSessions(t *testing.T) {
	for _, collision := range []string{"forked", "forked-i"} {
		for _, owner := range []string{"attached source", "ordinary route"} {
			t.Run(collision+"/"+owner, func(t *testing.T) {
				database := openTestDB(t)
				const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
				incoming := db.ArchiveSourceProvenance{Attachment: key, ManifestID: "incoming-manifest", Source: db.ArchiveSourceIdentity{
					ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "incoming",
				}}
				other := incoming
				other.Source.SourceKey, other.ManifestID = "other", "other-manifest"
				require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
				require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{
					{Provenance: incoming, Target: "target"}, {Provenance: other, Target: "target"},
				}))
				for _, native := range []string{"forked", "forked-i"} {
					provenance := &db.ArchiveSessionProvenance{ArchiveSourceProvenance: incoming, NativeSessionID: native}
					if native == collision {
						provenance.ArchiveSourceProvenance = other
						if owner == "ordinary route" {
							provenance = nil
						}
					}
					id := device + "~" + native
					_, err := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
						Session:           db.Session{ID: id, Agent: "claude", Machine: device, Project: "saved", FilePath: new("archive://saved/" + native)},
						Messages:          []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "keep stored " + native}},
						ArchiveProvenance: provenance, DataVersion: db.CurrentDataVersion(),
					}})
					require.NoError(t, err)
				}
				// A fixed timestamp makes any freshness mutation observable without
				// depending on the speed of the parse or the clock's resolution.
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET local_modified_at = '2020-01-01T00:00:00.000Z'`)
					return err
				}))
				before := make(map[string]*db.Session)
				beforeMessages := make(map[string][]db.Message)
				for _, native := range []string{"forked", "forked-i"} {
					id := device + "~" + native
					var err error
					before[id], err = database.GetSessionFull(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, before[id])
					beforeMessages[id], err = database.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
				}
				root := t.TempDir()
				path := filepath.Join(root, "project", "forked.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(newClaudeDAGBuilder(true).String()), 0o600))
				parent := NewEngine(t.Context(), database, EngineConfig{Ephemeral: true})
				t.Cleanup(parent.Close)
				stats, err := parent.SyncAttachedSource(t.Context(), incoming, "target", func() (EngineConfig, string, func() error, error) {
					return EngineConfig{
						AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
						Machine:   device, IDPrefix: device + "~", Ephemeral: true, ArchiveReparse: true,
						ArchiveWriteProvenance: func(_ *db.Session, native string) (db.ArchiveSessionProvenance, error) {
							return db.ArchiveSessionProvenance{ArchiveSourceProvenance: incoming, NativeSessionID: native}, nil
						},
					}, path, func() error { return nil }, nil
				}, nil)
				require.Error(t, err)
				assert.Positive(t, stats.Failed)
				for id, session := range before {
					after, err := database.GetSessionFull(t.Context(), id)
					require.NoError(t, err)
					assert.Equal(t, session, after, "a rejected DAG must not change any result's metadata")
					messages, err := database.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					assert.Equal(t, beforeMessages[id], messages)
				}
				pending, err := database.ArchiveSourcePending(t.Context(), incoming, "target")
				require.NoError(t, err)
				assert.True(t, pending)
			})
		}
	}
}

func TestOrdinarySourceCannotReplaceAttachedHistory(t *testing.T) {
	for _, mode := range []string{"parser exclusion", "rebuild replacement", "rebuild parser exclusion"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const id = device + "~owned"
			key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
			provenance := db.ArchiveSessionProvenance{
				Attachment:      key,
				Source:          db.ArchiveSourceIdentity{ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "source"},
				ManifestID:      "manifest",
				NativeSessionID: "owned",
			}
			require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
			require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{{Provenance: provenance.ArchiveSourceProvenance, Target: "target"}}))
			_, err := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session:         db.Session{ID: id, Project: "archived", Machine: device, Agent: "claude", FilePath: new("archive://source/owned.jsonl")},
				Messages:        []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "retained attached history"}},
				ReplaceMessages: true, ArchiveProvenance: &provenance,
			}})
			require.NoError(t, err)

			root := t.TempDir()
			path := filepath.Join(root, "project", "owned.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			content := "<command-name>/usage</command-name>\n<command-message>usage</command-message>\n<command-args></command-args>"
			if mode == "rebuild replacement" {
				content = "colliding contributor transcript"
			}
			require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", content).String()), 0o644))
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: device, IDPrefix: device + "~", Ephemeral: true}
			var stats SyncStats
			if mode == "parser exclusion" {
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				stats = engine.SyncAll(t.Context(), nil)
			} else {
				localRoot := t.TempDir()
				localPath := filepath.Join(localRoot, "project", "local.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(localPath), 0o755))
				require.NoError(t, os.WriteFile(localPath, []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "local history").String()), 0o644))
				engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {localRoot}}, Machine: "local"})
				t.Cleanup(engine.Close)
				stats, err = engine.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{Contributors: []RebuildContributor{{Name: "ordinary", Config: cfg}}})
				require.NoError(t, err)
				assert.True(t, stats.Aborted)
			}
			assert.Positive(t, stats.Failed)
			messages, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "retained attached history", messages[0].Content)
			ids, err := database.ArchiveSourceSessionIDs(t.Context(), provenance.Source)
			require.NoError(t, err)
			assert.Equal(t, []string{id}, ids)
		})
	}
}
