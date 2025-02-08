package runkube

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	log "log/slog"
	"net"
	"os"
	"path"
	"time"

	"github.com/go-jose/go-jose/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	externaljwt "k8s.io/externaljwt/apis/v1"
	kmsv2 "k8s.io/kms/apis/v2"
)

type jwksRef struct {
	path string
	jwks *jose.JSONWebKeySet
}

func (jkr *jwksRef) Read() error {
	log.Debug("jkr.read", "jkr.path", jkr.path)
	f, err := os.Open(jkr.path)
	if err != nil {
		log.Info("jkr.read", "err", err)
		return err
	}

	jkr.jwks = &jose.JSONWebKeySet{}
	if err := json.NewDecoder(f).Decode(jkr.jwks); err != nil {
		log.Debug("jkr.decode", "err", err)
		return err
	}

	return jkr.Validate()
}

func (jkr *jwksRef) Write(wantKid string, role string) error {
	log.Debug("jkr.write", "jkr.path", jkr.path, "jkr.role", role)
	err := jkr.Read()
	if err == nil {
		return nil
	}

	if jkr.jwks == nil {
		jkr.jwks = &jose.JSONWebKeySet{}
	}

	if k := jkr.jwks.Key(defaultKeyID); k != nil {
		log.Debug("jkr.write key is up to date", "jkr.path", jkr.path)
		return nil
	}

	var jwk jose.JSONWebKey

	switch role {
	case "encryption":
		kb := make([]byte, 32)
		n, err := rand.Read(kb)
		if err != nil || n != 32 {
			panic("crypto.Rand read failed")
		}

		jwk = jose.JSONWebKey{
			Key:       kb,
			Use:       "enc",
			Algorithm: string(jose.A256GCM),
			KeyID:     defaultKeyID,
		}
	case "signing":
		key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			return err
		}
		jwk = jose.JSONWebKey{
			Key:       key,
			Use:       "sig",
			Algorithm: string(jose.ES384),
			KeyID:     defaultKeyID,
		}
	default:
		return errors.New("unknown role " + role)
	}

	jkr.jwks.Keys = append(jkr.jwks.Keys, jwk)

	os.MkdirAll(path.Dir(jkr.path), 0700)
	w, err := os.OpenFile(jkr.path, os.O_WRONLY|os.O_CREATE, 0700)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(jkr.jwks)
}

func (jkr *jwksRef) Validate() error {
	if jkr.jwks == nil {
		return errors.New("jkr.jwks not set")
	}

	for _, k := range jkr.jwks.Keys {
		log.Debug("jkr.validate", "jkr.path", jkr.path, "kid", k.KeyID)
	}
	return nil
}

type kms struct {
	keySet *jwksRef
	kmsv2.UnimplementedKeyManagementServiceServer
}

const defaultKeyID = "default"

func (k *kms) Status(ctx context.Context, _ *kmsv2.StatusRequest) (*kmsv2.StatusResponse, error) {
	return &kmsv2.StatusResponse{
		Version: "v2",
		Healthz: "ok",
		KeyId:   defaultKeyID,
	}, nil
}

func (k *kms) Decrypt(ctx context.Context, req *kmsv2.DecryptRequest) (*kmsv2.DecryptResponse, error) {
	logger := log.With(log.Group("decrypt", "req.keyid", req.KeyId, "req.uid", req.Uid))
	parsed, err := jose.ParseEncrypted(
		string(req.GetCiphertext()),
		[]jose.KeyAlgorithm{jose.DIRECT},
		[]jose.ContentEncryption{jose.A256GCM},
	)
	if err != nil {
		logger.Debug("jose.parseEncrypted", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "parse ciphertext")
	}

	plain, err := parsed.Decrypt(k.keySet.jwks)
	if err != nil {
		logger.Debug("parsed.decrypt", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "decrypt")
	}
	return &kmsv2.DecryptResponse{
		Plaintext: plain,
	}, nil
}

func (k *kms) Encrypt(ctx context.Context, req *kmsv2.EncryptRequest) (*kmsv2.EncryptResponse, error) {
	logger := log.With(log.Group("encrypt", "req.uid", req.Uid))

	keys := k.keySet.jwks.Key(defaultKeyID)
	if len(keys) != 1 {
		return nil, status.Errorf(codes.FailedPrecondition, "one key expected, got %d", len(keys))
	}
	key := keys[0]

	enc, err := jose.NewEncrypter(jose.A256GCM, jose.Recipient{
		Algorithm: jose.DIRECT,
		Key:       key,
	}, nil)
	if err != nil {
		logger.Debug("jose.newEncrypter", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "crypto %v", err)
	}

	jwe, err := enc.Encrypt(req.Plaintext)
	if err != nil {
		logger.Debug("enc.Encrypt", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "encrypt %v", err)
	}

	encrypted, err := jwe.CompactSerialize()
	if err != nil {
		logger.Debug("jwe.compactSerialize", "err", err)
		return nil, status.Errorf(codes.FailedPrecondition, "serialize %v", err)
	}

	return &kmsv2.EncryptResponse{
		Ciphertext: []byte(encrypted),
		KeyId:      defaultKeyID,
	}, nil
}

var _ kmsv2.KeyManagementServiceServer = &kms{}

// RunKMSPlugin starts the KMS plugin gRPC server.
func (c *ControlPlane) RunKMSPlugin(ctx context.Context) error {
	p := c.unixSocketURL("kms")
	l, err := net.Listen("unix", p.Opaque)
	if err != nil {
		return err
	}

	s := grpc.NewServer(grpc.WaitForHandlers(true))

	ksr := c.keySetRef("encryption")
	if err := ksr.Read(); err != nil {
		return err
	}

	kmsv2.RegisterKeyManagementServiceServer(s, &kms{
		keySet: ksr,
	})

	return s.Serve(l)
}

type jwtsigner struct {
	keySet *jwksRef
	externaljwt.UnimplementedExternalJWTSignerServer
}

var _ externaljwt.ExternalJWTSignerServer = &jwtsigner{}

func (s *jwtsigner) Sign(ctx context.Context, req *externaljwt.SignJWTRequest) (*externaljwt.SignJWTResponse, error) {
	keys := s.keySet.jwks.Key(defaultKeyID)
	if len(keys) != 1 {
		return nil, status.Errorf(codes.FailedPrecondition, "one key expected, got %d", len(keys))
	}
	key := keys[0]

	header := &struct {
		Algorithm string `json:"alg,omitempty"`
		KeyID     string `json:"kid,omitempty"`
		Type      string `json:"typ,omitempty"`
	}{
		Type:      "JWT",
		Algorithm: key.Algorithm,
		KeyID:     key.KeyID,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, status.Convert(err).Err()
	}
	base64Header := base64.RawURLEncoding.EncodeToString(headerJSON)

	var hashAlg crypto.Hash
	var rsBytes int

	switch key.Algorithm {
	case "ES256":
		hashAlg = crypto.SHA256
		rsBytes = 32
	case "ES384":
		hashAlg = crypto.SHA384
		rsBytes = 48
	default:
		return nil, status.Convert(errors.New("unsupported signing algorithm")).Err()
	}

	toBeSignedHash := hashBytes(hashAlg, base64Header, ".", req.Claims)

	sigR, sigS, err := ecdsa.Sign(rand.Reader, key.Key.(*ecdsa.PrivateKey), toBeSignedHash)
	if err != nil {
		return nil, status.Convert(err).Err()
	}

	srb := make([]byte, rsBytes)
	ssb := make([]byte, rsBytes)

	sigR.FillBytes(srb)
	sigS.FillBytes(ssb)

	sigb := []byte{}
	sigb = append(sigb, srb...)
	sigb = append(sigb, ssb...)

	base64Signature := base64.RawURLEncoding.EncodeToString(sigb)

	return &externaljwt.SignJWTResponse{
		Header:    base64Header,
		Signature: base64Signature,
	}, nil
}

func hashBytes(h crypto.Hash, bs ...string) []byte {
	hasher := h.New()
	for _, b := range bs {
		hasher.Write([]byte(b))
	}
	return hasher.Sum(nil)
}

func (s *jwtsigner) FetchKeys(ctx context.Context, req *externaljwt.FetchKeysRequest) (*externaljwt.FetchKeysResponse, error) {
	eks := []*externaljwt.Key{}
	for _, jwk := range s.keySet.jwks.Keys {
		kb, err := x509.MarshalPKIXPublicKey(jwk.Public().Key)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "signer.fetchKeys %v", err)
		}

		eks = append(eks, &externaljwt.Key{
			KeyId: jwk.KeyID,
			Key:   kb,
		})
	}

	return &externaljwt.FetchKeysResponse{
		Keys:               eks,
		DataTimestamp:      timestamppb.New(time.Unix(0, 0)),
		RefreshHintSeconds: 600,
	}, nil
}

func (s *jwtsigner) Metadata(ctx context.Context, req *externaljwt.MetadataRequest) (*externaljwt.MetadataResponse, error) {
	return &externaljwt.MetadataResponse{
		MaxTokenExpirationSeconds: 3600,
	}, nil
}

// RunJWTSigner starts the JWT signer gRPC server.
func (c *ControlPlane) RunJWTSigner(ctx context.Context) error {
	ksr := c.keySetRef("serviceaccount")
	if err := ksr.Read(); err != nil {
		return err
	}
	signer := &jwtsigner{keySet: ksr}

	s := grpc.NewServer()
	externaljwt.RegisterExternalJWTSignerServer(s, signer)
	reflection.Register(s)

	p := c.unixSocketURL("jwtsigner")
	l, err := net.Listen("unix", p.Opaque)
	if err != nil {
		return err
	}

	return s.Serve(l)
}
