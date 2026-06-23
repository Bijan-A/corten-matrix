// corten-matrix — build-time interface placeholder (NOT functional logic).
//
// This file only exists so the tree compiles when the packaged build's
// delivery-identity precheck implementation is absent. It is intentionally
// trivial and carries no separate license or copyright claim. The real
// implementation is supplied at build time and is not part of this repository.

package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
)

// ensureIdentityKeys performs the outbound delivery-identity precheck for a
// Matrix→iMessage message before it is handed to the sender. It returns a
// non-nil response when the send has already been resolved and must not
// proceed, or (nil, nil) to continue with the normal send path.
//
// This placeholder performs no precheck; the packaged build supplies the real one.
func (c *IMClient) ensureIdentityKeys(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, nil
}
