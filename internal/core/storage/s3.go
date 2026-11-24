package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Options configura el backend S3.
type S3Options struct {
	Region string
	Bucket string
	// Endpoint solo en local (emulador). Vacio = AWS.
	Endpoint string
	// PublicEndpoint es el host de las URLs que abre el navegador; vacio =
	// Endpoint.
	PublicEndpoint string
	// AccessKey y SecretKey solo para el emulador local; vacios = cadena
	// estandar de credenciales del SDK (rol IAM en despliegue).
	AccessKey, SecretKey string
}

// S3 es el Backend sobre Amazon S3 (o un emulador compatible en local).
type S3 struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

// NewS3 crea el backend. Con Endpoint usa estilo de ruta (bucket en la ruta),
// que es lo que soportan los emuladores.
func NewS3(ctx context.Context, o S3Options) (*S3, error) {
	if o.Bucket == "" {
		return nil, errors.New("falta ARGOS_S3_BUCKET")
	}
	loaders := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.AccessKey != "" {
		loaders = append(loaders, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loaders...)
	if err != nil {
		return nil, fmt.Errorf("configuracion de AWS: %w", err)
	}
	withEndpoint := func(endpoint string) func(*s3.Options) {
		return func(opts *s3.Options) {
			if endpoint != "" {
				opts.BaseEndpoint = aws.String(endpoint)
				opts.UsePathStyle = true
			}
		}
	}
	public := o.PublicEndpoint
	if public == "" {
		public = o.Endpoint
	}
	return &S3{
		bucket:  o.Bucket,
		client:  s3.NewFromConfig(cfg, withEndpoint(o.Endpoint)),
		presign: s3.NewPresignClient(s3.NewFromConfig(cfg, withEndpoint(public))),
	}, nil
}

// EnsureBucket crea el bucket si no existe y le pone CORS para los origenes
// indicados (solo local: en despliegue lo hace la infraestructura con sus
// politicas). Un emulador que no soporte CORS por bucket no es un error: el
// suyo se configura por variable de entorno.
func (b *S3) EnsureBucket(ctx context.Context, origins []string) error {
	if _, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &b.bucket}); err != nil {
		_, err := b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &b.bucket})
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
			return fmt.Errorf("creando el bucket %s: %w", b.bucket, err)
		}
	}
	if len(origins) == 0 {
		return nil
	}
	_, _ = b.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: &b.bucket,
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			AllowedOrigins: origins,
			AllowedMethods: []string{"GET", "PUT", "HEAD"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(600),
		}}},
	})
	return nil
}

// PresignPut implementa Backend. Content-Type y Content-Length van firmados:
// S3 rechaza un archivo de otro tipo o tamano.
func (b *S3) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (Upload, error) {
	req, err := b.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket, Key: &key, ContentType: &contentType, ContentLength: &size,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Upload{}, fmt.Errorf("firmando la subida: %w", err)
	}
	headers := map[string]string{"Content-Type": contentType}
	return Upload{URL: req.URL, Method: req.Method, Headers: headers, ExpiresAt: time.Now().Add(ttl)}, nil
}

// PresignGet implementa Backend.
func (b *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := b.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &b.bucket, Key: &key},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("firmando la descarga: %w", err)
	}
	return req.URL, nil
}

// Head implementa Backend.
func (b *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("consultando el objeto: %w", err)
	}
	info := ObjectInfo{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}
	return info, nil
}

// PutObject implementa Backend.
func (b *S3) PutObject(ctx context.Context, key, contentType string, body []byte) error {
	size := int64(len(body))
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.bucket, Key: &key, Body: bytes.NewReader(body), ContentType: &contentType, ContentLength: &size,
	})
	if err != nil {
		return fmt.Errorf("subiendo el objeto: %w", err)
	}
	return nil
}

// Delete implementa Backend.
func (b *S3) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("borrando el objeto: %w", err)
	}
	return nil
}
