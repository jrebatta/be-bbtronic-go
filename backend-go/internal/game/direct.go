package game

import (
	"context"
	"fmt"
	"strings"
)

// Preguntas Directas. Reference: PreguntasDirectasService and
// GameSessionService.startRoundPlay/getRoundInfo.

func startDirect(ctx context.Context, env Env, r *Room) error {
	r.Activated = true // Java start() sets gameStarted=true
	g := newGame(env, KindDirect)
	g.RoundID = env.id()
	g.Phase = PhaseWaitingQuestions
	g.Direct = &DirectState{}
	if err := beginDirectRound(ctx, env, r); err != nil {
		return err
	}
	r.State.Game = g
	return nil
}

// beginDirectRound resets ready flags and the shown set (Java replaces the
// shownQuestions collection). Historic question rows are preserved.
func beginDirectRound(ctx context.Context, env Env, r *Room) error {
	resetReady(r)
	if err := env.Data.ClearShownQuestions(ctx, r.ID); err != nil {
		return fmt.Errorf("clearing shown questions: %w", err)
	}
	r.Direct = &DirectRound{Shown: map[int64]bool{}}
	return nil
}

func applyRoundNew(ctx context.Context, env Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd, KindDirect)
	if err != nil {
		return Outcome{}, err
	}
	if err := beginDirectRound(ctx, env, r); err != nil {
		return Outcome{}, err
	}
	g.RoundID = env.id()
	g.Phase = PhaseWaitingQuestions
	g.Direct.CurrentQuestionID = nil
	return Outcome{
		Result:  map[string]any{"roundId": g.RoundID, "phase": g.Phase},
		Changed: true, Reason: "round.started",
	}, nil
}

func applyQuestionSubmit(ctx context.Context, env Env, r *Room, actor *Member, cmd Command) (Outcome, error) {
	g, err := requireGame(r, cmd, KindDirect)
	if err != nil {
		return Outcome{}, err
	}
	if err := requireRound(g, cmd); err != nil {
		return Outcome{}, err
	}
	if g.Phase != PhaseWaitingQuestions {
		return Outcome{}, Errf(CodeStateConflict, "la ronda ya ha iniciado; no se pueden enviar más preguntas")
	}
	var p struct {
		ToPlayerID string `json:"toPlayerId"`
		Text       string `json:"text"`
		Anonymous  *bool  `json:"anonymous"`
	}
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return Outcome{}, err
	}
	if p.Anonymous == nil {
		return Outcome{}, Errf(CodeInvalidInput, "anonymous es obligatorio")
	}
	toID, err := ParseID(p.ToPlayerID, "toPlayerId")
	if err != nil {
		return Outcome{}, err
	}
	to := r.Member(toID)
	if to == nil {
		return Outcome{}, Errf(CodeInvalidInput, "el destinatario no está en la sala")
	}
	text, err := cleanText(p.Text, env.Rules.QuestionTextMaxRunes, "text")
	if err != nil {
		return Outcome{}, err
	}
	if len(r.Direct.Questions) >= env.Rules.MaxQuestionsPerRound {
		return Outcome{}, Errf(CodeLimitReached, "la ronda alcanzó el máximo de %d preguntas", env.Rules.MaxQuestionsPerRound)
	}
	q := DirectQuestion{From: actor.Username, To: to.Username, Text: text, Anonymous: *p.Anonymous}
	id, err := env.Data.InsertDirectQuestion(ctx, r.ID, g.RoundID, q)
	if err != nil {
		return Outcome{}, fmt.Errorf("inserting question: %w", err)
	}
	q.ID = id
	r.Direct.Questions = append(r.Direct.Questions, q)
	return Outcome{
		Result:  map[string]any{"questionId": FormatID(id)},
		Changed: true, Reason: "question.submitted",
	}, nil
}

func applyRoundCheckReady(ctx context.Context, env Env, r *Room, _ *Member, cmd Command) (Outcome, error) {
	var pending []*Member
	for _, m := range r.Members {
		if !m.Ready {
			pending = append(pending, m)
		}
	}
	allReady := len(r.Members) > 0 && len(pending) == 0
	if !allReady {
		msg, err := waitingMessage(ctx, env, pending)
		if err != nil {
			return Outcome{}, err
		}
		ids := make([]string, len(pending))
		for i, m := range pending {
			ids[i] = FormatID(m.ID)
		}
		return Outcome{Result: map[string]any{
			"allReady": false, "pendingPlayerIds": ids, "waitingMessage": msg, "started": false,
		}, Reason: "sync"}, nil
	}

	res := map[string]any{"allReady": true, "pendingPlayerIds": []string{}, "waitingMessage": nil, "started": false}
	g := r.State.Game
	if g == nil || g.Kind != KindDirect || g.Phase != PhaseWaitingQuestions {
		return Outcome{Result: res, Reason: "sync"}, nil
	}
	if _, err := requireGame(r, cmd); err != nil {
		return Outcome{}, err
	}
	if err := requireRound(g, cmd); err != nil {
		return Outcome{}, err
	}
	if len(r.Direct.Questions) == 0 {
		return Outcome{}, Errf(CodeNoQuestions, "no hay preguntas para jugar en esta ronda")
	}
	// startRoundPlay + the first getCurrentQuestion of Java: the question
	// with the lowest ID becomes current and is marked shown.
	first := r.Direct.Questions[0]
	if err := markShown(ctx, env, r, g, first.ID); err != nil {
		return Outcome{}, err
	}
	g.Phase = PhaseInProgress
	res["started"] = true
	return Outcome{Result: res, Changed: true, Reason: "round.playing"}, nil
}

func waitingMessage(ctx context.Context, env Env, pending []*Member) (string, error) {
	list := make([]string, len(pending))
	for i, m := range pending {
		list[i] = m.Username
	}
	names := strings.Join(list, ", ")
	templates, err := env.Data.WaitingTemplates(ctx)
	if err != nil {
		return "", fmt.Errorf("loading waiting templates: %w", err)
	}
	if len(templates) == 0 {
		return "Aún faltan usuarios por estar listos: " + names, nil
	}
	t := templates[env.Rand.IntN(len(templates))]
	return replaceAll(t, "{usuarios}", names), nil
}

func nextDirect(ctx context.Context, env Env, r *Room, g *Game, cmd Command) (Outcome, error) {
	if err := requireRound(g, cmd); err != nil {
		return Outcome{}, err
	}
	switch g.Phase {
	case PhaseCompleted:
		return Outcome{Result: map[string]any{"exhausted": true}, Reason: "questions.exhausted"}, nil
	case PhaseInProgress:
	default:
		return Outcome{}, Errf(CodeStateConflict, "la ronda aún no ha comenzado")
	}
	lastTo := ""
	if cur := r.currentDirect(); cur != nil {
		lastTo = cur.To
	}
	var candidates []DirectQuestion
	for _, q := range r.Direct.Questions {
		if !r.Direct.Shown[q.ID] && q.To != lastTo {
			candidates = append(candidates, q)
		}
	}
	if len(candidates) == 0 { // allow repeating the recipient when there is no alternative
		for _, q := range r.Direct.Questions {
			if !r.Direct.Shown[q.ID] {
				candidates = append(candidates, q)
			}
		}
	}
	if len(candidates) == 0 {
		g.Phase = PhaseCompleted // normalization of Java's allQuestionsShown
		return Outcome{Result: map[string]any{"exhausted": true}, Changed: true, Reason: "questions.exhausted"}, nil
	}
	next := candidates[env.Rand.IntN(len(candidates))]
	if err := markShown(ctx, env, r, g, next.ID); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: map[string]any{
		"questionId": FormatID(next.ID), "number": len(r.Direct.Shown), "total": len(r.Direct.Questions), "exhausted": false,
	}, Changed: true, Reason: "question.changed"}, nil
}

func markShown(ctx context.Context, env Env, r *Room, g *Game, id int64) error {
	if err := env.Data.AddShownQuestion(ctx, r.ID, id); err != nil {
		return fmt.Errorf("marking question shown: %w", err)
	}
	r.Direct.Shown[id] = true
	g.Direct.CurrentQuestionID = &id
	return nil
}

// currentDirect returns the selected Preguntas Directas question, if any.
func (r *Room) currentDirect() *DirectQuestion {
	g := r.State.Game
	if g == nil || g.Direct == nil || g.Direct.CurrentQuestionID == nil || r.Direct == nil {
		return nil
	}
	for i := range r.Direct.Questions {
		if r.Direct.Questions[i].ID == *g.Direct.CurrentQuestionID {
			return &r.Direct.Questions[i]
		}
	}
	return nil
}
