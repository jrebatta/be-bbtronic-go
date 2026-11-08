package game

import (
	"context"
	"fmt"
	"strings"
)

// Catalog games: Yo Nunca Nunca, Cultura Pendeja, Quién es más probable and
// Preguntas Incómodas. References: YoNuncaNuncaService, CulturaPendejaService,
// QuienEsMasProbableService, PreguntasIncomodasService.

func startCatalog(ctx context.Context, env Env, r *Room, kind Kind) error {
	items, err := env.Data.CatalogItems(ctx, kind)
	if err != nil {
		return fmt.Errorf("loading catalog %s: %w", kind, err)
	}
	if len(items) == 0 {
		return Errf(CodeCatalogEmpty, "no hay preguntas disponibles para %s", kind)
	}
	shuffle(env.Rand, items) // Collections.shuffle
	g := newGame(env, kind)
	g.RoundID = env.id()
	g.Phase = PhaseInProgress
	g.Catalog = &CatalogState{Remaining: items}
	if kind == KindUncomfortable {
		resetReady(r) // PreguntasIncomodasService.start resets ready
	}
	r.State.Game = g
	return nil
}

func nextCatalog(ctx context.Context, env Env, r *Room, g *Game, cmd Command) (Outcome, error) {
	var p struct {
		Type string `json:"type"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	typ := strings.TrimSpace(p.Type)
	if typ == "" {
		return Outcome{}, Errf(CodeInvalidInput, "type es obligatorio")
	}
	if len([]rune(typ)) > env.Rules.CatalogTypeMaxRunes {
		return Outcome{}, Errf(CodeInvalidInput, "type demasiado largo")
	}
	cs := g.Catalog
	for {
		if len(cs.Remaining) == 0 {
			changed := g.Phase != PhaseCompleted
			g.Phase = PhaseCompleted // whole catalog exhausted (Java allQuestionsShown)
			return Outcome{Result: map[string]any{"exhausted": true}, Changed: changed, Reason: "questions.exhausted"}, nil
		}
		idx := -1
		for i, it := range cs.Remaining {
			if strings.EqualFold(it.Type, typ) { // equalsIgnoreCase
				idx = i
				break
			}
		}
		if idx < 0 {
			// Only this type is exhausted: nothing is mutated (votes and the
			// current question are preserved — fixes the Probable vote loss).
			return Outcome{}, &Error{Code: CodeQuestionTypeExhausted,
				Message: "no hay más preguntas de ese tipo", Details: map[string]any{"type": typ}}
		}
		item := cs.Remaining[idx]
		text, found, err := env.Data.CatalogText(ctx, g.Kind, item.ID)
		if err != nil {
			return Outcome{}, fmt.Errorf("loading catalog text: %w", err)
		}
		cs.Remaining = append(cs.Remaining[:idx], cs.Remaining[idx+1:]...)
		if !found {
			continue // catalog row removed after start: skip it
		}
		q := &CatalogQuestion{InstanceID: env.id(), ID: item.ID, Type: item.Type, Text: text}
		switch g.Kind {
		case KindMostLikely:
			if strings.Contains(q.Text, "{player}") {
				m := drawFromBag(env, r, cs)
				q.Text = replaceAll(q.Text, "{player}", m.Username)
				q.ToPlayerID, q.ToPlayerName = &m.ID, m.Username
			}
			cs.Voting = &Voting{InstanceID: q.InstanceID} // votes cleared only on success
		case KindUncomfortable:
			m := drawFromBag(env, r, cs) // Java always draws a recipient
			q.Text = replaceAll(q.Text, "{player}", m.Username)
			q.ToPlayerID, q.ToPlayerName = &m.ID, m.Username
		}
		cs.Current = q
		return Outcome{Result: map[string]any{
			"questionInstanceId": q.InstanceID, "questionId": FormatID(q.ID), "exhausted": false,
		}, Changed: true, Reason: "question.changed"}, nil
	}
}

// drawFromBag picks a member without repetition until every current member
// has been drawn (C11: departed members are skipped, new members join the
// current cycle). The room always has at least the host when this runs.
func drawFromBag(env Env, r *Room, cs *CatalogState) *Member {
	served := cs.Served[:0:0]
	for _, id := range cs.Served {
		if r.Member(id) != nil {
			served = append(served, id)
		}
	}
	var eligible []*Member
	for _, m := range r.Members {
		if !containsID(served, m.ID) {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		served = nil
		eligible = r.Members
	}
	m := eligible[env.Rand.IntN(len(eligible))]
	cs.Served = append(served, m.ID)
	return m
}

func reconcileCatalogDeparture(env Env, r *Room, g *Game, id int64) {
	cs := g.Catalog
	cs.Served = removeID(cs.Served, id)
	if g.Kind == KindMostLikely {
		reconcileProbableDeparture(env, r, cs, id)
	}
}

func replaceAll(s, old, repl string) string { return strings.ReplaceAll(s, old, repl) }
