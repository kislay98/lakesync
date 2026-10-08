package core

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

// Property keys under which a checkpoint is stored in a table's snapshot summary
// and table properties. The prefix is the one place the product name appears in
// Iceberg metadata.
const (
	CheckpointPrefix       = AppName + "."
	PropBookmark           = CheckpointPrefix + "bookmark"
	PropSeq                = CheckpointPrefix + "seq"
	PropGeneration         = CheckpointPrefix + "generation"
	PropPhase              = CheckpointPrefix + "phase"
	PropSnapshotCursor     = CheckpointPrefix + "snapshot_cursor"
	PropRunID              = CheckpointPrefix + "run_id"
	PropCheckpointVersion  = CheckpointPrefix + "checkpoint_version"
	checkpointVersionValue = "1"
)

// Properties encodes the checkpoint as string properties.
func (c Checkpoint) Properties() map[string]string {
	return map[string]string{
		PropCheckpointVersion: checkpointVersionValue,
		PropBookmark:          base64.StdEncoding.EncodeToString(c.Bookmark),
		PropSeq:               strconv.FormatInt(c.Seq, 10),
		PropGeneration:        strconv.FormatInt(c.Generation, 10),
		PropPhase:             string(c.Phase),
		PropSnapshotCursor:    string(c.SnapshotCursor),
		PropRunID:             c.RunID,
	}
}

// CheckpointFromProperties decodes a checkpoint; ok is false when the properties
// carry no checkpoint at all.
func CheckpointFromProperties(p map[string]string) (cp Checkpoint, ok bool, err error) {
	if p == nil {
		return cp, false, nil
	}
	seqStr, has := p[PropSeq]
	if !has {
		return cp, false, nil
	}
	cp.Seq, err = strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return cp, false, fmt.Errorf("checkpoint: bad %s %q: %w", PropSeq, seqStr, err)
	}
	if g := p[PropGeneration]; g != "" {
		cp.Generation, err = strconv.ParseInt(g, 10, 64)
		if err != nil {
			return cp, false, fmt.Errorf("checkpoint: bad %s %q: %w", PropGeneration, g, err)
		}
	}
	if b := p[PropBookmark]; b != "" {
		cp.Bookmark, err = base64.StdEncoding.DecodeString(b)
		if err != nil {
			return cp, false, fmt.Errorf("checkpoint: bad %s: %w", PropBookmark, err)
		}
	}
	cp.Phase = Phase(p[PropPhase])
	cp.SnapshotCursor = SnapshotCursor(p[PropSnapshotCursor])
	cp.RunID = p[PropRunID]
	return cp, true, nil
}
