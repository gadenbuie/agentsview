package remotesync

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestRemoteClaudeDAGCollisionPreservesAttachedFreshness(t *testing.T) {
	for _, collision := range []string{"forked", "forked-i"} {
		t.Run(collision, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			const device = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			key := db.ArchiveAttachmentKey{RepositoryID: "repository", SnapshotID: "snapshot"}
			owner := db.ArchiveSourceProvenance{Attachment: key, ManifestID: "manifest", Source: db.ArchiveSourceIdentity{
				ArchiveID: "archive", DeviceID: device, RootID: "root", SourceKey: "source",
			}}
			require.NoError(t, database.RegisterArchiveAttachment(t.Context(), key, "archive"))
			require.NoError(t, database.SetArchiveAttachmentTargets(t.Context(), key, "target", []db.ArchiveSourceTarget{{Provenance: owner, Target: "target"}}))
			for _, native := range []string{"forked", "forked-i"} {
				var provenance *db.ArchiveSessionProvenance
				if native == collision {
					provenance = &db.ArchiveSessionProvenance{ArchiveSourceProvenance: owner, NativeSessionID: native}
				}
				id := device + "~" + native
				_, err := database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
					Session:           db.Session{ID: id, Agent: "claude", Machine: device, Project: "saved"},
					Messages:          []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "keep stored " + native}},
					ArchiveProvenance: provenance, DataVersion: db.CurrentDataVersion(),
				}})
				require.NoError(t, err)
			}
			require.NoError(t, database.CompleteArchiveSource(t.Context(), owner, "target"))
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
			body := testjsonl.NewSessionBuilder().
				AddClaudeUserWithUUID("2026-01-01T00:00:00Z", "start", "a", "").
				AddClaudeAssistantWithUUID("2026-01-01T00:00:01Z", "reply", "b", "a").
				AddClaudeUserWithUUID("2026-01-01T00:00:02Z", "one", "c", "b").
				AddClaudeAssistantWithUUID("2026-01-01T00:00:03Z", "one reply", "d", "c").
				AddClaudeUserWithUUID("2026-01-01T00:00:04Z", "two", "e", "d").
				AddClaudeAssistantWithUUID("2026-01-01T00:00:05Z", "two reply", "f", "e").
				AddClaudeUserWithUUID("2026-01-01T00:00:06Z", "three", "g", "f").
				AddClaudeAssistantWithUUID("2026-01-01T00:00:07Z", "three reply", "h", "g").
				AddClaudeUserWithUUID("2026-01-01T00:00:08Z", "four", "k", "h").
				AddClaudeAssistantWithUUID("2026-01-01T00:00:09Z", "four reply", "l", "k").
				AddClaudeUserWithUUID("2026-01-01T00:01:00Z", "fork", "i", "b").
				AddClaudeAssistantWithUUID("2026-01-01T00:01:01Z", "fork reply", "j", "i").String()
			root := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(root, "remote", "project", "forked.jsonl"), []byte(body))
			stats, err := (Importer{Host: device, DB: database, Full: true, RequireComplete: true}).ImportExtracted(
				t.Context(), TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {"/remote"}}}, root,
			)
			require.Error(t, err)
			assert.Positive(t, stats.Failed)
			for id, session := range before {
				after, err := database.GetSessionFull(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, session, after, "a rejected remote DAG must preserve every result's metadata")
				messages, err := database.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, beforeMessages[id], messages)
			}
			status, err := database.ArchiveAttachmentStatus(t.Context(), key)
			require.NoError(t, err)
			assert.Zero(t, status.Pending)
		})
	}
}
