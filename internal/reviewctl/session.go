package reviewctl

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// SessionReviewer adapts a durable review session to the controller's seam.
//
// A named type rather than an interface satisfied directly by *reviewrun.Session,
// so the narrowing is visible: the session exposes a durable record and a
// startup survey that the controller has no business reading, and a policy layer
// that could reach an execution fact is one refactor away from routing on it.
type SessionReviewer struct{ session *reviewrun.Session }

// Over wraps a session.
func Over(s *reviewrun.Session) *SessionReviewer { return &SessionReviewer{session: s} }

func (r *SessionReviewer) Review(ctx context.Context, sub reviewrun.Subject) (review.Report, error) {
	return r.session.Review(ctx, sub)
}

func (r *SessionReviewer) Retire(ctx context.Context, sub reviewrun.Subject) error {
	return r.session.Retire(ctx, sub)
}
