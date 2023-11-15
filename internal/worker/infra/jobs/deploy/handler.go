package deploy

import (
	"context"
	"fmt"

	deplcmd "github.com/YuukanOO/seelf/internal/deployment/app/command"
	depldomain "github.com/YuukanOO/seelf/internal/deployment/domain"
	"github.com/YuukanOO/seelf/internal/worker/domain"
	"github.com/YuukanOO/seelf/internal/worker/infra/jobs"
	"github.com/YuukanOO/seelf/pkg/log"
	"github.com/YuukanOO/seelf/pkg/monad"
	"github.com/YuukanOO/seelf/pkg/types"
)

type (
	Request depldomain.DeploymentCreated

	handler struct {
		logger log.Logger
		deploy func(context.Context, deplcmd.DeployCommand) error
	}
)

func New(logger log.Logger, deploy func(context.Context, deplcmd.DeployCommand) error) jobs.Handler {
	return &handler{
		logger: logger,
		deploy: deploy,
	}
}

func (*handler) CanPrepare(data any) bool            { return types.Is[Request](data) }
func (*handler) CanProcess(data domain.JobData) bool { return types.Is[Data](data) }

func (h *handler) Prepare(payload any) (domain.JobData, monad.Maybe[string], error) {
	evt, ok := payload.(Request)

	if !ok {
		return nil, monad.None[string](), domain.ErrInvalidPayload
	}

	data := Data{evt.ID.AppID(), evt.ID.DeploymentNumber()}
	dedupeName := monad.Value(fmt.Sprintf("%s.%s", data.Discriminator(), evt.Config.ProjectName()))

	return data, dedupeName, nil
}

func (h *handler) Process(ctx context.Context, job domain.Job) error {
	data, ok := job.Data().(Data)

	if !ok {
		return domain.ErrInvalidPayload
	}

	// Here the error is not given back to the worker because if it fails, the information
	// is already in the associated Deployment. The only exception is for sql errors.
	if err := h.deploy(ctx, deplcmd.DeployCommand{
		AppID:            string(data.AppID),
		DeploymentNumber: int(data.DeploymentNumber),
	}); err != nil {
		h.logger.Errorw("deploy job has failed",
			"error", err,
			"appid", data.AppID,
			"deployment", data.DeploymentNumber,
		)
	}

	return nil
}