package e2b

import "context"

// Stop has envd send the supervisor SIGTERM, which the supervisor passes on to the command once.
// Nothing follows the signal. A sandbox that is gone, one whose command has ended and one whose
// command was never started have no supervisor to signal, and that is not an error.
func (p *Provider) Stop(ctx context.Context, id string) error {
	inside, exists, err := p.inside(ctx, id)
	if err != nil || !exists {
		return err
	}
	return inside.signal(ctx, supervisorTag)
}
