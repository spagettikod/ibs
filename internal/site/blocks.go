package site

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/keys"
)

// storeBlock stores an encrypted block of a bucket on a local node, as part
// of the upload in progress (upload): until the upload commits it, the
// garbage collector leaves it alone, however long the upload takes.
func (s *Site) storeBlock(ctx context.Context, bucket, upload, id string, ct []byte) error {
	unlock := s.lockBlock(id)
	defer unlock()
	now := time.Now().Unix()
	var nodeID string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO blocks(id, bucket, size, refs, node_id, touched, verified, upload, age)
		VALUES(?, ?, ?, 0, '', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET touched = excluded.touched, upload = CASE WHEN refs <= 0 THEN excluded.upload ELSE upload END
		RETURNING node_id`, id, bucket, len(ct), now, now, upload, keys.IsAge(ct)).Scan(&nodeID)
	if err != nil {
		return err
	}
	if nodeID != "" && s.nodeStore(nodeID) != nil {
		return nil
	}
	nodeID, err = s.putOwn(ctx, blockstore.OwnNS, id, ct)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE blocks SET node_id = ?, verified = ? WHERE id = ?`, nodeID, now, id)
	return err
}

// readPiece returns the ciphertext of a file's piece of a shared block. It
// reads only that piece from the node holding the block, checked against the
// piece's hash; otherwise it reads the whole block (which fetches it from
// another site and heals it when it's missing or damaged here) and takes the
// piece from it.
func (s *Site) readPiece(ctx context.Context, ref BlockRef) ([]byte, error) {
	var nodeID string
	s.DB.QueryRowContext(ctx, `SELECT node_id FROM blocks WHERE id = ?`, ref.ID).Scan(&nodeID)
	if st := s.nodeStore(nodeID); nodeID != "" && st != nil {
		if ct, err := blockstore.GetRange(ctx, st, blockstore.OwnNS, ref.ID, ref.Off, ref.Len); err == nil && blockcodec.Verify(ref.Piece, ct) {
			return ct, nil
		}
	}
	ct, err := s.readBlock(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	if ref.Off+ref.Len > int64(len(ct)) || !blockcodec.Verify(ref.Piece, ct[ref.Off:ref.Off+ref.Len]) {
		return nil, fmt.Errorf("piece %s of block %s: %w", ref.Piece[:12], ref.ID[:12], blockcodec.ErrCorrupt)
	}
	return ct[ref.Off : ref.Off+ref.Len], nil
}

// readBlock returns the ciphertext of one of our blocks. It reads the local
// copy first and falls back to replicas on other sites, healing the local
// copy when it was missing or corrupt.
func (s *Site) readBlock(ctx context.Context, id string) ([]byte, error) {
	var nodeID string
	if err := s.DB.QueryRowContext(ctx, `SELECT node_id FROM blocks WHERE id = ?`, id).Scan(&nodeID); err != nil {
		return nil, fmt.Errorf("block %s: %w", id[:12], err)
	}
	if nodeID != "" {
		if store := s.nodeStore(nodeID); store != nil {
			ct, err := store.Get(ctx, blockstore.OwnNS, id)
			if err == nil && blockcodec.Verify(id, ct) {
				return ct, nil
			}
			if err == nil || errors.Is(err, blockstore.ErrNotFound) {
				// The local copy is gone or damaged: forget it so it gets healed.
				s.log.Warn("local block missing or corrupt", "block", id[:12], "node", nodeID)
				s.DB.Exec(`UPDATE blocks SET node_id = '' WHERE id = ? AND node_id = ?`, id, nodeID)
				nodeID = ""
			}
		}
	}
	ct, site, err := s.fetchFromReplicas(ctx, id)
	if err != nil {
		return nil, err
	}
	if nodeID == "" {
		if err := s.healBlock(ctx, id, ct, ""); err != nil {
			s.log.Warn("heal block", "block", id[:12], "err", err)
		} else {
			s.log.Info("healed block from replica", "block", id[:12], "from", site[:8])
		}
	}
	return ct, nil
}

// healBlock stores a copy fetched from a replica on a local node, provided
// the block is still assigned to from ("" = missing locally, or a lost node).
func (s *Site) healBlock(ctx context.Context, id string, ct []byte, from string) error {
	unlock := s.lockBlock(id)
	defer unlock()
	var nodeID string
	var refs int
	if err := s.DB.QueryRow(`SELECT node_id, refs FROM blocks WHERE id = ?`, id).Scan(&nodeID, &refs); err != nil {
		return err
	}
	if nodeID != from || refs <= 0 {
		return nil
	}
	nodeID, err := s.putOwn(ctx, blockstore.OwnNS, id, ct)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE blocks SET node_id = ?, verified = ? WHERE id = ?`, nodeID, time.Now().Unix(), id)
	s.notify(TopicReplication)
	return err
}

// fetchFromReplicas tries every site recorded as holding the block, most
// recently seen first.
func (s *Site) fetchFromReplicas(ctx context.Context, id string) ([]byte, string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.site_id FROM replicas r JOIN peers p ON p.site_id = r.site_id
		WHERE r.block_id = ? AND r.state = 'ok' ORDER BY p.online DESC, p.last_seen DESC`, id)
	if err != nil {
		return nil, "", err
	}
	var sites []string
	for rows.Next() {
		var sid string
		rows.Scan(&sid)
		sites = append(sites, sid)
	}
	rows.Close()
	if len(sites) == 0 {
		return nil, "", fmt.Errorf("block %s is not available locally and has no replicas", id[:12])
	}
	var lastErr error
	for _, sid := range sites {
		ct, err := s.peerGetBlock(ctx, sid, id)
		if err == nil && blockcodec.Verify(id, ct) {
			return ct, sid, nil
		}
		if err == nil {
			err = blockcodec.ErrCorrupt
		}
		if errors.Is(err, blockstore.ErrNotFound) || errors.Is(err, blockcodec.ErrCorrupt) {
			s.DB.Exec(`DELETE FROM replicas WHERE block_id = ? AND site_id = ?`, id, sid)
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("block %s unavailable on all replicas: %w", id[:12], lastErr)
}
