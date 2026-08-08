package acp

import (
	"context"

	acpsdk "github.com/coder/acp-go-sdk"
)

type outbound interface {
	update(context.Context, acpsdk.SessionId, acpsdk.SessionUpdate) error
	requestPermission(
		context.Context,
		acpsdk.RequestPermissionRequest,
	) (acpsdk.RequestPermissionResponse, error)
	elicit(context.Context, createElicitationRequest) (createElicitationResponse, error)
}

type connectionOutbound struct {
	connection *acpsdk.Connection
}

func (o connectionOutbound) update(
	ctx context.Context,
	sessionID acpsdk.SessionId,
	update acpsdk.SessionUpdate,
) error {
	return o.connection.SendNotification(ctx, acpsdk.ClientMethodSessionUpdate, acpsdk.SessionNotification{
		SessionId: sessionID,
		Update:    update,
	})
}

func (o connectionOutbound) requestPermission(
	ctx context.Context,
	request acpsdk.RequestPermissionRequest,
) (acpsdk.RequestPermissionResponse, error) {
	return acpsdk.SendRequest[acpsdk.RequestPermissionResponse](
		o.connection,
		ctx,
		acpsdk.ClientMethodSessionRequestPermission,
		request,
	)
}

func (o connectionOutbound) elicit(
	ctx context.Context,
	request createElicitationRequest,
) (createElicitationResponse, error) {
	return acpsdk.SendRequest[createElicitationResponse](
		o.connection,
		ctx,
		acpsdk.ClientMethodElicitationCreate,
		request,
	)
}
