package conversion

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

// Public Apple PKI root, downloaded from https://www.apple.com/certificateauthority/AppleRootCA-G3.cer.
//
//go:embed apple-root-g3.pem
var appleRootPEM []byte

func hasOID(cert *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oid) {
			return true
		}
	}
	return false
}

func (v *StoreVerifier) verifyApple(app App, p MobilePurchase, now time.Time) (VerifiedPurchase, error) {
	var result VerifiedPurchase
	parts := strings.Split(p.SignedTransaction, ".")
	if len(parts) != 3 || len(p.SignedTransaction) > 60000 {
		return result, errInvalidPurchase
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return result, errInvalidPurchase
	}
	var header struct {
		Alg   string   `json:"alg"`
		Chain []string `json:"x5c"`
		Crit  []string `json:"crit"`
	}
	if json.Unmarshal(headerBytes, &header) != nil || header.Alg != "ES256" || len(header.Chain) != 3 || len(header.Crit) != 0 {
		return result, errInvalidPurchase
	}
	certs := make([]*x509.Certificate, 3)
	for i, encoded := range header.Chain {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return result, errInvalidPurchase
		}
		certs[i], err = x509.ParseCertificate(der)
		if err != nil {
			return result, errInvalidPurchase
		}
	}
	if !hasOID(certs[0], asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}) || !hasOID(certs[1], asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}) {
		return result, errInvalidPurchase
	}
	var transaction struct {
		ID          string `json:"transactionId"`
		Bundle      string `json:"bundleId"`
		Product     string `json:"productId"`
		Environment string `json:"environment"`
		Purchased   int64  `json:"purchaseDate"`
		Signed      int64  `json:"signedDate"`
		Revoked     *int64 `json:"revocationDate"`
		Price       *int64 `json:"price"`
		Currency    string `json:"currency"`
		Quantity    int    `json:"quantity"`
		Ownership   string `json:"inAppOwnershipType"`
		OfferType   *int   `json:"offerType"`
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payloadBytes, &transaction) != nil {
		return result, errInvalidPurchase
	}
	// Offline verification uses the signed date for certificate validity, as Apple's
	// verifier does with online checks disabled. No live revocation/refund polling.
	if transaction.Signed < 946684800000 || transaction.Signed > now.Add(5*time.Minute).UnixMilli() {
		return result, errInvalidPurchase
	}
	roots := v.appleRoots
	if roots == nil {
		roots = x509.NewCertPool()
		roots.AppendCertsFromPEM(appleRootPEM)
	}
	intermediates := x509.NewCertPool()
	intermediates.AddCert(certs[1])
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: time.UnixMilli(transaction.Signed), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return result, errInvalidPurchase
	}
	publicKey, ok := certs[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() || certs[0].KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return result, errInvalidPurchase
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return result, errInvalidPurchase
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return result, errInvalidPurchase
	}
	if transaction.ID == "" || transaction.Bundle != app.Mobile.BundleID || transaction.Product != p.ProductID || transaction.Environment != "Production" || transaction.Revoked != nil || transaction.Ownership != "PURCHASED" || transaction.OfferType != nil || transaction.Price == nil || *transaction.Price < 0 || transaction.Quantity < 1 || !currencyPattern.MatchString(transaction.Currency) || transaction.Purchased < 946684800000 || transaction.Purchased > transaction.Signed {
		return result, errInvalidPurchase
	}
	return VerifiedPurchase{ID: transaction.ID, ProductID: transaction.Product, Currency: transaction.Currency, Timestamp: transaction.Purchased, Value: float64(*transaction.Price) / 1000, Quantity: transaction.Quantity}, nil
}
