/*
Copyright (c) Facebook, Inc. and its affiliates.
All rights reserved.

This source code is licensed under the BSD-style license found in the
LICENSE file in the root directory of this source tree.
*/

// package service implements the core of bootstrapper
package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"github.com/emakeev/snowflake"
	"github.com/golang/protobuf/ptypes"
	"log"
	"os"
	"sync"
	"time"

	"magma/orc8r/lib/go/protos"
	"magma/orc8r/lib/go/security/cert"
	"magma/orc8r/lib/go/security/key"
)

const (
	PERIODIC_BOOTSTRAP_CHECK_INTERVAL = time.Minute * 15
	PREEXPIRY_BOOTSTRAP_INTERVAL      = time.Hour * 20
	BOOTSTRAP_RETRY_INTERVAL          = time.Second * 90

	PrivateKeyType          = "P384"
	CertificateECKeyType    = PrivateKeyType
	DefaultTLSBootstrapPort = 443
)

// Bootstrapper - implementation of bootstrapper
type Bootstrapper struct {
	sync.RWMutex `json:"-"`

	// GW Hardware ID
	HardwareId string
	// Challenge Key params
	ChallengeKeyFile string
	// Configs loaded from control_proxy.yml
	CpConfig ControlProxyCfg
	// BootstrapCompletionNotifier is an optional callback function which will be called on every
	// successful or failed bootstrap completion.
	// NOTE: it's responsibility of the notifier to lock/unlock passed *Bootstrapper if it needs
	// to access (read/write) it's fields. The bootstrapper object will not be locked prior to
	// the notify callback.
	BootstrapCompletionNotifier func(*Bootstrapper, error) `json:"-"`
	//
	// private, caches, tmps
	//
	// 'cached' challenge key
	challengeKey *ecdsa.PrivateKey
	// if set to true - start bootstrapping even is the GW certificate is still valid
	forceBootstrap bool
}

// NewBootstrapper returns a new instance of bootstrapper with initialized configuration
func NewBootstrapper() *Bootstrapper {
	b := NewDefaultBootsrapper()
	b.updateBootstrapperKeyCfg()
	b.updateFromControlProxyCfg()
	return b
}

// Initialize loads HW ID & challenge key and verifies it's validity
func (b *Bootstrapper) Initialize() error {
	if b == nil {
		return fmt.Errorf("Invalid (nil) Bootstrapper")
	}
	hwId, err := snowflake.Make()
	if err != nil {
		return fmt.Errorf("Bootstrapper failed to get gateway Hardware ID: %v", err)
	}
	b.Lock()
	defer b.Unlock()

	b.HardwareId = hwId.String()

	privKey, err := b.getChallengeKey()
	if err != nil {
		return err
	}
	var ok bool
	b.challengeKey, ok = privKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("invalid Bootstrapper challenge key type: %T, expected: %T, try to remove & re-run",
			privKey, b.challengeKey)
	}
	return nil
}

// Start loads challenge key and starts bootstrapper routine
// Start will block if successful and is supposed to run in its own routine
// Start will try to generate a new bootstrapper key if it doesn't exist only during an initial
// run & will try to only read it during periodic checks.
// It'll return an error if either initial run or periodic check fail, it's up to a caller to decide
// whether to fail on an error or retry
func (b *Bootstrapper) Start() error {
	if b == nil {
		return fmt.Errorf("Invalid (nil) Bootstrapper")
	}
	if b.challengeKey == nil {
		err := b.Initialize()
		if err != nil {
			return err
		}
	}
	// initial check & bootstrap
	if err := b.PeriodicCheck(time.Now()); err != nil {
		return err
	}
	// Start certificate update loop
	c := time.Tick(PERIODIC_BOOTSTRAP_CHECK_INTERVAL)
	for now := range c {
		if err := b.PeriodicCheck(now); err != nil {
			return err
		}
	}
	return nil
}

// PeriodicCheck verifies GW certificate validity and bootstraps GW if needed
func (b *Bootstrapper) PeriodicCheck(now time.Time) (err error) {
	b.RLock()
	notifier := b.BootstrapCompletionNotifier
	defer func() {
		if notifier != nil {
			notifier(b, err)
		}
	}()
	if b.validateCert(now) {
		b.RUnlock()
		return // all good, cert is still valid - return
	}
	// Need a new certificate, bootstrap using cloud
	newCert, newCertKey, err := b.bootstrap()
	// Save the new cert & key
	certFile, certKeyFile := b.CpConfig.GwCertFile, b.CpConfig.GwCertKeyFile
	b.RUnlock()

	if err != nil {
		return
	}
	oldCertFile, oldCertKeyFile, newCertFile, newCertKeyFile :=
		certFile+".old", certKeyFile+".old", certFile+".new", certKeyFile+".new"

	if err = key.WriteKey(newCertKeyFile, newCertKey); err != nil {
		return
	}
	var certOut *os.File
	certOut, err = os.Create(newCertFile)
	if err != nil {
		err = fmt.Errorf("Failed to open %s for writing: %v", newCertFile, err)
		return
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: newCert.CertDer})
	certOut.Close()

	// swap old and new certificates & keys
	cerr := os.Rename(certFile, oldCertFile)
	kerr := os.Rename(certKeyFile, oldCertKeyFile)

	if err = os.Rename(newCertFile, certFile); err != nil {
		if cerr == nil { // roll back cert
			os.Rename(oldCertFile, certFile)
		}
		if kerr == nil { // roll back key
			os.Rename(oldCertKeyFile, certKeyFile)
		}
		err = fmt.Errorf("Failed to rename new certificate %s: %v", newCertFile, err)
		return err
	}
	if err = os.Rename(newCertKeyFile, certKeyFile); err != nil {
		if cerr == nil { // roll back cert
			os.Rename(oldCertFile, certFile)
		}
		if kerr == nil { // roll back key
			os.Rename(oldCertKeyFile, certKeyFile)
		}
		err = fmt.Errorf("Failed to rename new certificate key %s: %v", newCertKeyFile, err)
		return err
	}
	// Done updating certificate
	log.Printf("Successfully bootstrapped gateway '%s' with new certificate: %s and key: %s",
		b.HardwareId, certFile, certKeyFile)

	// bootstrapped, return
	err = nil
	return err
}

// ForceBootstrap runs Bootstrapping sequence irregardless of GW certificate freshness
func (b *Bootstrapper) ForceBootstrap() error {
	if b == nil {
		return fmt.Errorf("Invalid (nil) Bootstrapper")
	}

	b.Lock()
	b.forceBootstrap = true
	b.challengeKey = nil
	b.Unlock()

	err := b.Initialize()
	if err != nil {
		return err
	}
	if err = b.PeriodicCheck(time.Now()); err != nil {
		return err
	}

	b.Lock()
	b.forceBootstrap = false
	b.Unlock()

	return nil
}

// RefreshConfigs attempts to re-read all bootsrapper configs, it also invalidates challenge key cache: challengeKey
func (b *Bootstrapper) RefreshConfigs() {
	if b == nil {
		b.Lock()
		b.updateBootstrapperKeyCfg()
		b.updateFromControlProxyCfg()
		b.challengeKey = nil
		b.Unlock()
	}
}

// bootstrap generates new gateway key & CSR, reaches to the cloud to sign the CSR and returns new cert & key
// NOTE: it's a responsibility of a caller to synchronise access to Bootstrapper when calling Bootstrap
func (b *Bootstrapper) bootstrap() (*protos.Certificate, interface{}, error) {
	var err error

	if b.challengeKey == nil {
		if err = b.updateChallengeKey(); err != nil {
			return nil, nil, err
		}
	}
	// Generate a new gateway cert key & CSR
	newCertKey, csr, err := createCSRAndKey(b.HardwareId)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create new certificate: %v", err)
	}

	// Complete challenge based auth & sign CSR
	conn, err := b.GetBootstrapperCloudConnection()
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	client := protos.NewBootstrapperClient(conn)
	challenge, err := client.GetChallenge(context.Background(), &protos.AccessGatewayID{Id: b.HardwareId})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get Bootstrapper challenge: %v", err)
	}
	if challenge.KeyType != protos.ChallengeKey_SOFTWARE_ECDSA_SHA256 {
		return nil, nil, fmt.Errorf("unsupported Bootstrapper challenge type: %s", challenge.KeyType.String())
	}
	hashed := sha256.Sum256(challenge.Challenge)
	r, s, err := ecdsa.Sign(rand.Reader, b.challengeKey, hashed[:])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to sign challenge: %v", err)
	}
	// create challenge response
	resp := &protos.Response{
		HwId:      &protos.AccessGatewayID{Id: b.HardwareId},
		Challenge: challenge.Challenge,
		Response: &protos.Response_EcdsaResponse{
			EcdsaResponse: &protos.Response_ECDSA{R: r.Bytes(), S: s.Bytes()},
		},
		Csr: &protos.CSR{
			Id:        protos.NewGatewayIdentity(b.HardwareId, "", ""),
			ValidTime: ptypes.DurationProto(PREEXPIRY_BOOTSTRAP_INTERVAL * 5),
			CsrDer:    csr,
			CertType:  protos.CertType_DEFAULT,
		},
	}
	newCert, err := client.RequestSign(context.Background(), resp)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to sign CSR: %v", err)
	}
	return newCert, newCertKey, nil
}

func (b *Bootstrapper) validateCert(now time.Time) bool {
	if b.forceBootstrap {
		return false // Force Bootstrap
	}
	crt, err := cert.LoadCert(b.CpConfig.GwCertFile)
	if err != nil {
		log.Printf("Failed to load certificate & key from '%s', '%s'; error: %v; will bootstrap",
			b.CpConfig.GwCertFile, b.CpConfig.GwCertKeyFile, err)
		return false
	}
	// Loaded cert, check expiry time
	preBootstrapInterval := PREEXPIRY_BOOTSTRAP_INTERVAL
	certDuration := crt.NotAfter.Sub(crt.NotBefore)
	if certDuration > 0 && certDuration <= preBootstrapInterval {
		newInterval := certDuration / 4
		if newInterval >= PERIODIC_BOOTSTRAP_CHECK_INTERVAL {
			preBootstrapInterval = newInterval
		}
	}
	if (crt.NotBefore.Before(now) || crt.NotBefore.Equal(now)) &&
		crt.NotAfter.Sub(now) >= preBootstrapInterval {
		// Certificate is still valid, continue
		return true
	}
	log.Printf("Certificate is valid from %s to %s, current time is %s; will bootstrap",
		crt.NotBefore, crt.NotAfter, now)

	return false
}

func (b *Bootstrapper) updateChallengeKey() error {
	privKey, err := key.ReadKey(b.ChallengeKeyFile)
	if err != nil {
		return fmt.Errorf("Bootstrapper ReadKey(%s) error: %v", b.ChallengeKeyFile, err)
	}
	var ok bool
	b.challengeKey, ok = privKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("invalid Bootstrapper challenge key type: %T, expected: %T", privKey, b.challengeKey)
	}
	return nil
}
