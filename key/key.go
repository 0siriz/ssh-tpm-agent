package key

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"

	"github.com/foxboron/ssh-tpm-agent/utils"
	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"golang.org/x/crypto/ssh"
)

// We need to know if the TPM handle has a pin set
type PINStatus uint8

const (
	NoPIN PINStatus = iota
	HasPIN
)

func (p PINStatus) String() string {
	switch p {
	case NoPIN:
		return "NoPIN"
	case HasPIN:
		return "HasPIN"
	}
	return "Not a PINStatus"
}

type Key struct {
	Version uint8
	PIN     PINStatus
	Type    tpm2.TPMAlgID
	Private tpm2.TPM2BPrivate
	Public  tpm2.TPM2BPublic
}

func (k *Key) ecdsaPubKey() (*ecdsa.PublicKey, error) {
	c := tpm2.BytesAs2B[tpm2.TPMTPublic](k.Public.Bytes())
	pub, err := c.Contents()
	if err != nil {
		return nil, err
	}
	ecc, err := pub.Unique.ECC()
	if err != nil {
		return nil, err
	}

	ecdsaKey := &ecdsa.PublicKey{Curve: elliptic.P256(),
		X: big.NewInt(0).SetBytes(ecc.X.Buffer),
		Y: big.NewInt(0).SetBytes(ecc.Y.Buffer),
	}

	return ecdsaKey, nil
}

func (k *Key) rsaPubKey() (*rsa.PublicKey, error) {
	pub, err := k.Public.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed getting content: %v", err)
	}
	rsaDetail, err := pub.Parameters.RSADetail()
	if err != nil {
		return nil, fmt.Errorf("failed getting rsa details: %v", err)
	}
	rsaUnique, err := pub.Unique.RSA()
	if err != nil {
		return nil, fmt.Errorf("failed getting unique rsa: %v", err)
	}

	return tpm2.RSAPub(rsaDetail, rsaUnique)
}

func (k *Key) PublicKey() (any, error) {
	switch k.Type {
	case tpm2.TPMAlgECDSA:
		return k.ecdsaPubKey()
	case tpm2.TPMAlgRSA:
		return k.rsaPubKey()
	}
	return nil, fmt.Errorf("no public key")
}

func (k *Key) SSHPublicKey() (ssh.PublicKey, error) {
	pubkey, err := k.PublicKey()
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pubkey)
}

func UnmarshalKey(b []byte) (*Key, error) {
	var key Key

	r := bytes.NewBuffer(b)

	for _, k := range []interface{}{
		&key.Version,
		&key.PIN,
		&key.Type,
	} {
		if err := binary.Read(r, binary.BigEndian, k); err != nil {
			return nil, err
		}
	}

	public, err := tpm2.Unmarshal[tpm2.TPM2BPublic](r.Bytes())
	if err != nil {
		return nil, err
	}

	private, err := tpm2.Unmarshal[tpm2.TPM2BPrivate](r.Bytes()[len(public.Bytes())+2:])
	if err != nil {
		return nil, err
	}

	key.Public = *public
	key.Private = *private

	return &key, err
}

func MarshalKey(k *Key) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, k.Version)
	binary.Write(&b, binary.BigEndian, k.PIN)
	binary.Write(&b, binary.BigEndian, k.Type)

	var pub []byte
	pub = append(pub, tpm2.Marshal(k.Public)...)
	pub = append(pub, tpm2.Marshal(k.Private)...)
	b.Write(pub)

	return b.Bytes()
}

var (
	keyType = "TPM PRIVATE KEY"
)

func EncodeKey(k *Key) []byte {
	data := MarshalKey(k)

	var buf bytes.Buffer
	pem.Encode(&buf, &pem.Block{
		Type:  keyType,
		Bytes: data,
	})
	return buf.Bytes()
}

func DecodeKey(pemBytes []byte) (*Key, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("not an armored key")
	}
	switch block.Type {
	case "TPM PRIVATE KEY":
		return UnmarshalKey(block.Bytes)
	default:
		return nil, fmt.Errorf("tpm-ssh: unsupported key type %q", block.Type)
	}
}

// Creates a Storage Key, or return the loaded storage key
func CreateSRK(tpm transport.TPMCloser, keytype tpm2.TPMAlgID) (*tpm2.AuthHandle, *tpm2.TPMTPublic, error) {

	var public tpm2.TPM2BPublic
	switch keytype {
	case tpm2.TPMAlgECDSA:
		public = tpm2.New2B(tpm2.ECCSRKTemplate)
	case tpm2.TPMAlgRSA:
		public = tpm2.New2B(tpm2.RSASRKTemplate)

	}

	srk := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHOwner,
		InSensitive: tpm2.TPM2BSensitiveCreate{
			Sensitive: &tpm2.TPMSSensitiveCreate{
				UserAuth: tpm2.TPM2BAuth{
					Buffer: []byte(nil),
				},
			},
		},
		InPublic: public,
	}

	var rsp *tpm2.CreatePrimaryResponse
	rsp, err := srk.Execute(tpm)
	if err != nil {
		return nil, nil, fmt.Errorf("failed creating primary key: %v", err)
	}

	srkPublic, err := rsp.OutPublic.Contents()
	if err != nil {
		return nil, nil, fmt.Errorf("failed getting srk public content: %v", err)
	}

	return &tpm2.AuthHandle{
		Handle: rsp.ObjectHandle,
		Name:   rsp.Name,
		Auth:   tpm2.PasswordAuth(nil),
	}, srkPublic, nil
}

var (
	eccPublic = tpm2.New2B(tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgECC,
		NameAlg: tpm2.TPMAlgSHA256,
		ObjectAttributes: tpm2.TPMAObject{
			SignEncrypt:         true,
			FixedTPM:            true,
			FixedParent:         true,
			SensitiveDataOrigin: true,
			UserWithAuth:        true,
		},
		Parameters: tpm2.NewTPMUPublicParms(
			tpm2.TPMAlgECC,
			&tpm2.TPMSECCParms{
				CurveID: tpm2.TPMECCNistP256,
				Scheme: tpm2.TPMTECCScheme{
					Scheme: tpm2.TPMAlgECDSA,
					Details: tpm2.NewTPMUAsymScheme(
						tpm2.TPMAlgECDSA,
						&tpm2.TPMSSigSchemeECDSA{
							HashAlg: tpm2.TPMAlgSHA256,
						},
					),
				},
			},
		),
	})

	rsaPublic = tpm2.New2B(tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgRSA,
		NameAlg: tpm2.TPMAlgSHA256,
		ObjectAttributes: tpm2.TPMAObject{
			SignEncrypt:         true,
			FixedTPM:            true,
			FixedParent:         true,
			SensitiveDataOrigin: true,
			UserWithAuth:        true,
		},
		Parameters: tpm2.NewTPMUPublicParms(
			tpm2.TPMAlgRSA,
			&tpm2.TPMSRSAParms{
				Scheme: tpm2.TPMTRSAScheme{
					Scheme: tpm2.TPMAlgRSASSA,
					Details: tpm2.NewTPMUAsymScheme(
						tpm2.TPMAlgRSASSA,
						&tpm2.TPMSSigSchemeRSASSA{
							HashAlg: tpm2.TPMAlgSHA256,
						},
					),
				},
				KeyBits: 2048,
			},
		),
	})
)

func CreateKey(tpm transport.TPMCloser, keytype tpm2.TPMAlgID, pin []byte) (*Key, error) {
	switch keytype {
	case tpm2.TPMAlgECDSA:
	case tpm2.TPMAlgRSA:
	default:
		return nil, fmt.Errorf("unsupported key type")
	}

	srkHandle, srkPublic, err := CreateSRK(tpm, keytype)
	if err != nil {
		return nil, fmt.Errorf("failed creating SRK: %v", err)
	}

	defer utils.FlushHandle(tpm, srkHandle)

	var keyPublic tpm2.TPM2BPublic

	switch keytype {
	case tpm2.TPMAlgECDSA:
		keyPublic = eccPublic
	case tpm2.TPMAlgRSA:
		keyPublic = rsaPublic
	}

	// Template for en ECDSA key for signing
	createKey := tpm2.Create{
		ParentHandle: srkHandle,
		InPublic:     keyPublic,
	}

	pinstatus := NoPIN

	if !bytes.Equal(pin, []byte("")) {
		createKey.InSensitive = tpm2.TPM2BSensitiveCreate{
			Sensitive: &tpm2.TPMSSensitiveCreate{
				UserAuth: tpm2.TPM2BAuth{
					Buffer: pin,
				},
			},
		}
		pinstatus = HasPIN
	}

	var createRsp *tpm2.CreateResponse
	createRsp, err = createKey.Execute(tpm,
		tpm2.HMAC(tpm2.TPMAlgSHA256, 16,
			tpm2.AESEncryption(128, tpm2.EncryptIn),
			tpm2.Salted(srkHandle.Handle, *srkPublic)))
	if err != nil {
		return nil, fmt.Errorf("failed creating TPM key: %v", err)
	}

	return &Key{
		Version: 1,
		PIN:     pinstatus,
		Type:    keytype,
		Private: createRsp.OutPrivate,
		Public:  createRsp.OutPublic,
	}, nil
}

func LoadKeyWithParent(tpm transport.TPMCloser, parent tpm2.AuthHandle, key *Key) (*tpm2.AuthHandle, error) {
	loadBlobCmd := tpm2.Load{
		ParentHandle: parent,
		InPrivate:    key.Private,
		InPublic:     key.Public,
	}
	loadBlobRsp, err := loadBlobCmd.Execute(tpm)
	if err != nil {
		return nil, fmt.Errorf("failed getting handle: %v", err)
	}

	// Return a AuthHandle with a nil PasswordAuth
	return &tpm2.AuthHandle{
		Handle: loadBlobRsp.ObjectHandle,
		Name:   loadBlobRsp.Name,
		Auth:   tpm2.PasswordAuth(nil),
	}, nil
}

func LoadKey(tpm transport.TPMCloser, key *Key) (*tpm2.AuthHandle, error) {
	srkHandle, _, err := CreateSRK(tpm, key.Type)
	if err != nil {
		return nil, err
	}

	defer utils.FlushHandle(tpm, srkHandle)

	return LoadKeyWithParent(tpm, *srkHandle, key)
}
