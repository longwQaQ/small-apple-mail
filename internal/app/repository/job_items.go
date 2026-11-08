package repository

import (
	"context"
	"database/sql"
	"time"
)

type JobItem struct {
	ID        int64     `json:"id"`
	Target    string    `json:"target"`
	TargetID  string    `json:"target_id"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) AddJobItem(ctx context.Context, jobID, targetID, target, status, message string, total int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO job_items(job_id,target_id,target,status,message,created_at) VALUES(?,?,?,?,?,?)", jobID, targetID, target, status, message, nowText()); err != nil {
		return err
	}
	if total > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET progress=MIN(100, (SELECT COUNT(*) FROM job_items WHERE job_id=?)*100/?) WHERE id=?`, jobID, total, jobID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) JobItems(ctx context.Context, jobID string) ([]JobItem, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,target_id,target,status,message,created_at FROM job_items WHERE job_id=? ORDER BY id", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobItem{}
	for rows.Next() {
		var item JobItem
		var created sql.NullString
		if err = rows.Scan(&item.ID, &item.TargetID, &item.Target, &item.Status, &item.Message, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(created)
		out = append(out, item)
	}
	return out, rows.Err()
}
