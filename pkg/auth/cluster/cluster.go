package cluster

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/lestrrat-go/backoff/v2"
	corev1 "github.com/rancher/opni/pkg/apis/core/v1"
	"github.com/rancher/opni/pkg/auth"
	"github.com/rancher/opni/pkg/b2mac"
	"github.com/rancher/opni/pkg/ecdh"
	"github.com/rancher/opni/pkg/keyring"
	"github.com/rancher/opni/pkg/logger"
	"github.com/rancher/opni/pkg/storage"
	"github.com/rancher/opni/pkg/util"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type (
	clusterIDKeyType  string
	sharedKeysKeyType string
)

const (
	ClusterIDKey  clusterIDKeyType  = "cluster_auth_cluster_id"
	SharedKeysKey sharedKeysKeyType = "cluster_auth_shared_keys"
)

type ClusterMiddleware struct {
	keyringStoreBroker storage.KeyringStoreBroker
	fakeKeyringStore   storage.KeyringStore
	headerKey          string
	logger             *zap.SugaredLogger
}

var _ auth.Middleware = (*ClusterMiddleware)(nil)

func New(ctx context.Context, keyringStore storage.KeyringStoreBroker, headerKey string) (*ClusterMiddleware, error) {
	lg := logger.New(
		logger.WithSampling(&zap.SamplingConfig{
			Initial:    1,
			Thereafter: 0,
		}),
	).Named("auth").Named("cluster")
	fakeKeyringStore, err := initFakeKeyring(ctx, keyringStore, lg)
	if err != nil {
		return nil, fmt.Errorf("failed to set up keyring store: %w", err)
	}

	return &ClusterMiddleware{
		keyringStoreBroker: keyringStore,
		fakeKeyringStore:   fakeKeyringStore,
		headerKey:          headerKey,
		logger:             lg,
	}, nil
}

var fakeKeyring keyring.Keyring

func initFakeKeyring(
	ctx context.Context,
	broker storage.KeyringStoreBroker,
	lg *zap.SugaredLogger,
) (storage.KeyringStore, error) {
	store, err := broker.KeyringStore("gateway-internal", &corev1.Reference{
		Id: "fake",
	})
	if err != nil {
		return nil, err
	}

	kp1 := ecdh.NewEphemeralKeyPair()
	kp2 := ecdh.NewEphemeralKeyPair()
	sec, err := ecdh.DeriveSharedSecret(kp1, ecdh.PeerPublicKey{
		PublicKey: kp2.PublicKey,
		PeerType:  ecdh.PeerTypeClient,
	})
	if err != nil {
		return nil, err
	}
	fakeKeyring = keyring.New(keyring.NewSharedKeys(sec))
	go func() {
		p := backoff.Exponential(
			backoff.WithMaxRetries(0),
			backoff.WithMinInterval(10*time.Millisecond),
			backoff.WithMaxInterval(10*time.Second),
			backoff.WithMultiplier(2.0),
		)
		bctx, ca := context.WithCancel(ctx)
		defer ca()
		b := p.Start(bctx)
		// print a warning every 10 failed attempts
		numFailedAttempts := 0
		for backoff.Continue(b) {
			ctx, ca := context.WithTimeout(bctx, 1*time.Second)
			defer ca()
			err := store.Put(ctx, fakeKeyring)
			if err == nil {
				if numFailedAttempts > 0 {
					lg.Infof("storage backend recovered after %d failed attempts", numFailedAttempts)
				}
				break
			}
			numFailedAttempts++
			if numFailedAttempts%10 == 0 {
				lg.With(
					"lastError", err,
					"attempt", numFailedAttempts,
				).Warn("the storage backend appears to be unresponsive, will continue to retry")
			}
		}
	}()
	return store, nil
}

func (m *ClusterMiddleware) doFakeKeyringVerify(mac []byte, id []byte, nonce uuid.UUID, payload []byte) {
	fakeKeyring, err := m.fakeKeyringStore.Get(context.Background())
	if err != nil {
		m.logger.Errorf("failed to get fake keyring: %v", err)
		return
	}
	fakeKeyring.Try(func(shared *keyring.SharedKeys) {
		b2mac.Verify(mac, id, nonce, payload, shared.ClientKey)
	})
}

func (m *ClusterMiddleware) Handle(c *fiber.Ctx) error {
	lg := m.logger
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		lg.Debug("unauthorized: authorization header required")
		return c.Status(fiber.StatusBadRequest).SendString("authorization header required")
	}

	code, clusterID, sharedKeys := m.doKeyringVerify(authHeader, c.Body())
	if code != http.StatusOK {
		return c.SendStatus(code)
	}

	c.Request().Header.Add(m.headerKey, string(clusterID))
	c.Locals(string(SharedKeysKey), sharedKeys)
	c.Locals(string(ClusterIDKey), string(clusterID))
	return c.Next()
}

func AuthorizedKeys(c *fiber.Ctx) *keyring.SharedKeys {
	return c.Locals(string(SharedKeysKey)).(*keyring.SharedKeys)
}

func AuthorizedID(c *fiber.Ctx) string {
	return c.Locals(string(ClusterIDKey)).(string)
}

func StreamAuthorizedKeys(ctx context.Context) *keyring.SharedKeys {
	return ctx.Value(SharedKeysKey).(*keyring.SharedKeys)
}

func StreamAuthorizedID(ctx context.Context) string {
	return ctx.Value(ClusterIDKey).(string)
}

func (m *ClusterMiddleware) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, ok := metadata.FromIncomingContext(ss.Context())
		if !ok {
			return grpc.Errorf(codes.InvalidArgument, "no metadata in context")
		}
		authHeader := md.Get(m.headerKey)
		if len(authHeader) > 0 && authHeader[0] == "" {
			return grpc.Errorf(codes.InvalidArgument, "authorization header required")
		}

		code, clusterID, sharedKeys := m.doKeyringVerify(authHeader[0], []byte(info.FullMethod))

		switch code {
		case http.StatusOK:
		case http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, http.StatusText(code))
		case http.StatusBadRequest:
			return status.Error(codes.InvalidArgument, http.StatusText(code))
		case http.StatusInternalServerError:
			return status.Error(codes.Internal, http.StatusText(code))
		default:
			return status.Error(codes.Unknown, http.StatusText(code))
		}

		ctx := context.WithValue(ss.Context(), SharedKeysKey, sharedKeys)
		ctx = context.WithValue(ctx, ClusterIDKey, string(clusterID))

		return handler(srv, &util.ServerStreamWithContext{
			Stream: ss,
			Ctx:    ctx,
		})
	}
}

func (m *ClusterMiddleware) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			err = grpc.Errorf(codes.InvalidArgument, "no metadata in context")
			return
		}
		authHeader := md.Get(m.headerKey)
		if len(authHeader) > 0 && authHeader[0] == "" {
			err = grpc.Errorf(codes.InvalidArgument, "authorization header required")
			return
		}

		code, clusterID, sharedKeys := m.doKeyringVerify(authHeader[0], []byte(info.FullMethod))

		switch code {
		case http.StatusOK:
		case http.StatusUnauthorized:
			err = status.Error(codes.Unauthenticated, http.StatusText(code))
			return
		case http.StatusBadRequest:
			err = status.Error(codes.InvalidArgument, http.StatusText(code))
			return
		case http.StatusInternalServerError:
			err = status.Error(codes.Internal, http.StatusText(code))
			return
		default:
			err = status.Error(codes.Unknown, http.StatusText(code))
		}

		ctx = context.WithValue(ctx, SharedKeysKey, sharedKeys)
		ctx = context.WithValue(ctx, ClusterIDKey, string(clusterID))
		return handler(ctx, req)
	}
}

func (m *ClusterMiddleware) doKeyringVerify(authHeader string, msgBody []byte) (int, string, *keyring.SharedKeys) {
	lg := m.logger
	clusterID, nonce, mac, err := b2mac.DecodeAuthHeader(authHeader)
	if err != nil {
		return http.StatusBadRequest, "", nil
	}

	ks, err := m.keyringStoreBroker.KeyringStore("gateway", &corev1.Reference{
		Id: string(clusterID),
	})
	if err != nil {
		lg.Debugf("unauthorized: error looking up keyring store for cluster %s: %v", clusterID, err)
		m.doFakeKeyringVerify(mac, clusterID, nonce, msgBody)
		return http.StatusUnauthorized, "", nil
	}

	kr, err := ks.Get(context.Background())
	if err != nil {
		lg.Debugf("unauthorized: error looking up keyring for cluster %s: %v", clusterID, err)
		m.doFakeKeyringVerify(mac, clusterID, nonce, msgBody)
		return http.StatusUnauthorized, "", nil
	}

	authorized := false
	var sharedKeys *keyring.SharedKeys
	if ok := kr.Try(func(shared *keyring.SharedKeys) {
		if err := b2mac.Verify(mac, clusterID, nonce, msgBody, shared.ClientKey); err == nil {
			authorized = true
			sharedKeys = shared
		}
	}); !ok {
		lg.Errorf("unauthorized: invalid or corrupted keyring for cluster %s: %v", clusterID, err)
		return http.StatusInternalServerError, "", nil
	}
	if !authorized {
		lg.Debugf("unauthorized: invalid mac for cluster %s", clusterID)
		return http.StatusUnauthorized, "", nil
	}

	return http.StatusOK, string(clusterID), sharedKeys
}
