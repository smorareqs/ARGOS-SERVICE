package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/files"
)

type presignInput struct {
	Body files.PresignRequest
}

type presignOutput struct {
	Body files.PresignResponse
}

type filePath struct {
	FileID uuid.UUID `path:"file_id" format:"uuid"`
}

type fileOutput struct {
	Body files.FileRead
}

type fileURLOutput struct {
	Body files.URLResponse
}

func callerOf(p auth.Principal) files.Caller {
	return files.Caller{TenantID: p.Membership.TenantID, UserID: p.User.ID, Permissions: p.Permissions}
}

// Archivos de la empresa activa en S3 (fase01.md §3.8). El permiso depende
// del tipo de archivo (files.Rules).
func registerFiles(v1 huma.API, svc *files.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/files")
	grp.UseMiddleware(guard.RequireTenant()...)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"archivos"}
		op.Security = api.Secured
	})

	huma.Register(grp, huma.Operation{
		OperationID: "presign-upload", Method: http.MethodPost, Path: "/presign-upload",
		Summary: "Pedir URL de subida",
		Description: "Registra el archivo en pending y devuelve una URL PUT de 15 minutos con Content-Type y tamano " +
			"firmados. Despues de subir, llamar a /files/{id}/confirm.",
		Errors: []int{http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *presignInput) (*presignOutput, error) {
		out, err := svc.PresignUpload(ctx, callerOf(p), in.Body)
		return &presignOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "confirm-upload", Method: http.MethodPost, Path: "/{file_id}/confirm",
		Summary: "Confirmar la subida", Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *filePath) (*fileOutput, error) {
		out, err := svc.Confirm(ctx, callerOf(p), in.FileID)
		return &fileOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "file-url", Method: http.MethodGet, Path: "/{file_id}/url",
		Summary: "URL de descarga (5 minutos)", Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *filePath) (*fileURLOutput, error) {
		out, err := svc.DownloadURL(ctx, callerOf(p), in.FileID)
		return &fileURLOutput{Body: out}, err
	}))
}
