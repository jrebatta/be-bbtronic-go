package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jrebatta/be-bbtronic-go/backend-go/internal/game"
)

// catalogTables maps game kinds to their fixed catalog tables. Table names are
// constants, never client input (plan 8.2).
var catalogTables = map[game.Kind]string{
	game.KindNeverEver:     "yo_nunca_nunca",
	game.KindTrivia:        "cultura_pendeja",
	game.KindMostLikely:    "quien_es_mas_probable",
	game.KindUncomfortable: "preguntas_incomodas",
}

// Data adapts a transaction to game.Data.
func (t *Tx) Data() game.Data { return txData{t} }

type txData struct{ t *Tx }

func table(kind game.Kind) (string, error) {
	tbl, ok := catalogTables[kind]
	if !ok {
		return "", fmt.Errorf("no catalog table for %q", kind)
	}
	return tbl, nil
}

func (d txData) CatalogItems(ctx context.Context, kind game.Kind) ([]game.CatalogItem, error) {
	tbl, err := table(kind)
	if err != nil {
		return nil, err
	}
	rows, err := d.t.tx.Query(ctx, `SELECT id, tipo FROM `+tbl+` ORDER BY id`) // tbl from constant map
	if err != nil {
		return nil, fmt.Errorf("querying catalog: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (game.CatalogItem, error) {
		var it game.CatalogItem
		err := row.Scan(&it.ID, &it.Type)
		return it, err
	})
}

func (d txData) CatalogText(ctx context.Context, kind game.Kind, id int64) (string, bool, error) {
	tbl, err := table(kind)
	if err != nil {
		return "", false, err
	}
	var text string
	err = d.t.tx.QueryRow(ctx, `SELECT texto FROM `+tbl+` WHERE id = $1`, id).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading catalog text: %w", err)
	}
	return text, true, nil
}

func (d txData) ImpostorWords(ctx context.Context) ([]string, error) {
	rows, err := d.t.tx.Query(ctx, `SELECT word FROM impostor_word ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying impostor words: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (d txData) WaitingTemplates(ctx context.Context) ([]string, error) {
	rows, err := d.t.tx.Query(ctx, `SELECT template FROM waiting_messages ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("querying waiting messages: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (d txData) InsertDirectQuestion(ctx context.Context, roomID int64, roundID string, q game.DirectQuestion) (int64, error) {
	var id int64
	err := d.t.tx.QueryRow(ctx, `INSERT INTO question (from_user, to_user, question, anonymous, round_id, game_session_id)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, q.From, q.To, q.Text, q.Anonymous, roundID, roomID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("inserting question: %w", err)
	}
	return id, nil
}

func (d txData) AddShownQuestion(ctx context.Context, roomID, questionID int64) error {
	_, err := d.t.tx.Exec(ctx, `INSERT INTO shown_questions (session_id, question_id)
		SELECT $1, $2 WHERE NOT EXISTS (SELECT 1 FROM shown_questions WHERE session_id = $1 AND question_id = $2)`, roomID, questionID)
	if err != nil {
		return fmt.Errorf("adding shown question: %w", err)
	}
	return nil
}

func (d txData) ClearShownQuestions(ctx context.Context, roomID int64) error {
	if _, err := d.t.tx.Exec(ctx, `DELETE FROM shown_questions WHERE session_id = $1`, roomID); err != nil {
		return fmt.Errorf("clearing shown questions: %w", err)
	}
	return nil
}
