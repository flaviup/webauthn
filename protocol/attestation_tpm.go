package protocol

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-tpm/tpm2"

	"github.com/flaviup/webauthn/metadata"
	"github.com/flaviup/webauthn/protocol/webauthncose"
)

var tpmAttestationKey = "tpm"

func init() {
	RegisterAttestationFormat(tpmAttestationKey, verifyTPMFormat)
}

func verifyTPMFormat(att AttestationObject, clientDataHash []byte) (string, []interface{}, error) {
	// Given the verification procedure inputs attStmt, authenticatorData
	// and clientDataHash, the verification procedure is as follows

	// Verify that attStmt is valid CBOR conforming to the syntax defined
	// above and perform CBOR decoding on it to extract the contained fields

	ver, present := att.AttStatement["ver"].(string)
	if !present {
		return "", nil, ErrAttestationFormat.WithDetails("Error retrieving ver value")
	}

	if ver != "2.0" {
		return "", nil, ErrAttestationFormat.WithDetails("WebAuthn only supports TPM 2.0 currently")
	}

	alg, present := att.AttStatement["alg"].(int64)
	if !present {
		return "", nil, ErrAttestationFormat.WithDetails("Error retrieving alg value")
	}

	coseAlg := webauthncose.COSEAlgorithmIdentifier(alg)

	x5c, x509present := att.AttStatement["x5c"].([]interface{})
	if !x509present {
		// Handle Basic Attestation steps for the x509 Certificate
		return "", nil, ErrNotImplemented
	}

	_, ecdaaKeyPresent := att.AttStatement["ecdaaKeyId"].([]byte)
	if ecdaaKeyPresent {
		return "", nil, ErrNotImplemented
	}

	sigBytes, present := att.AttStatement["sig"].([]byte)
	if !present {
		return "", nil, ErrAttestationFormat.WithDetails("Error retrieving sig value")
	}

	certInfoBytes, present := att.AttStatement["certInfo"].([]byte)
	if !present {
		return "", nil, ErrAttestationFormat.WithDetails("Error retrieving certInfo value")
	}

	pubAreaBytes, present := att.AttStatement["pubArea"].([]byte)
	if !present {
		return "", nil, ErrAttestationFormat.WithDetails("Error retrieving pubArea value")
	}

	// Verify that the public key specified by the parameters and unique fields of pubArea
	// is identical to the credentialPublicKey in the attestedCredentialData in authenticatorData.
	pubArea, err := tpm2.DecodePublic(pubAreaBytes)
	if err != nil {
		return "", nil, ErrAttestationFormat.WithDetails("Unable to decode TPMT_PUBLIC in attestation statement")
	}

	key, err := webauthncose.ParsePublicKey(att.AuthData.AttData.CredentialPublicKey)
	if err != nil {
		return "", nil, err
	}

	switch k := key.(type) {
	case webauthncose.EC2PublicKeyData:
		if pubArea.ECCParameters.CurveID != k.TPMCurveID() ||
			!bytes.Equal(pubArea.ECCParameters.Point.XRaw, k.XCoord) ||
			!bytes.Equal(pubArea.ECCParameters.Point.YRaw, k.YCoord) {
			return "", nil, ErrAttestationFormat.WithDetails("Mismatch between ECCParameters in pubArea and credentialPublicKey")
		}
	case webauthncose.RSAPublicKeyData:
		exp := uint32(k.Exponent[0]) + uint32(k.Exponent[1])<<8 + uint32(k.Exponent[2])<<16
		if !bytes.Equal(pubArea.RSAParameters.ModulusRaw, k.Modulus) ||
			pubArea.RSAParameters.Exponent() != exp {
			return "", nil, ErrAttestationFormat.WithDetails("Mismatch between RSAParameters in pubArea and credentialPublicKey")
		}
	default:
		return "", nil, ErrUnsupportedKey
	}

	// Concatenate authenticatorData and clientDataHash to form attToBeSigned
	attToBeSigned := append(att.RawAuthData, clientDataHash...)

	// Validate that certInfo is valid:
	// 1/4 Verify that magic is set to TPM_GENERATED_VALUE, handled here
	certInfo, err := tpm2.DecodeAttestationData(certInfoBytes)
	if err != nil {
		return "", nil, err
	}

	// 2/4 Verify that type is set to TPM_ST_ATTEST_CERTIFY.
	if certInfo.Type != tpm2.TagAttestCertify {
		return "", nil, ErrAttestationFormat.WithDetails("Type is not set to TPM_ST_ATTEST_CERTIFY")
	}

	// 3/4 Verify that extraData is set to the hash of attToBeSigned using the hash algorithm employed in "alg".
	f := webauthncose.HasherFromCOSEAlg(coseAlg)
	h := f()

	h.Write(attToBeSigned)
	if !bytes.Equal(certInfo.ExtraData, h.Sum(nil)) {
		return "", nil, ErrAttestationFormat.WithDetails("ExtraData is not set to hash of attToBeSigned")
	}

	// 4/4 Verify that attested contains a TPMS_CERTIFY_INFO structure as specified in
	// [TPMv2-Part2] section 10.12.3, whose name field contains a valid Name for pubArea,
	// as computed using the algorithm in the nameAlg field of pubArea
	// using the procedure specified in [TPMv2-Part1] section 16.
	matches, err := certInfo.AttestedCertifyInfo.Name.MatchesPublic(pubArea)
	if err != nil {
		return "", nil, err
	}

	if !matches {
		return "", nil, ErrAttestationFormat.WithDetails("Hash value mismatch attested and pubArea")
	}

	// Note that the remaining fields in the "Standard Attestation Structure"
	// [TPMv2-Part1] section 31.2, i.e., qualifiedSigner, clockInfo and firmwareVersion
	// are ignored. These fields MAY be used as an input to risk engines.

	// If x5c is present, this indicates that the attestation type is not ECDAA.
	if x509present {
		// In this case:
		// Verify the sig is a valid signature over certInfo using the attestation public key in aikCert with the algorithm specified in alg.
		aikCertBytes, valid := x5c[0].([]byte)
		if !valid {
			return "", nil, ErrAttestation.WithDetails("Error getting certificate from x5c cert chain")
		}

		aikCert, err := x509.ParseCertificate(aikCertBytes)
		if err != nil {
			return "", nil, ErrAttestationFormat.WithDetails("Error parsing certificate from ASN.1")
		}

		sigAlg := webauthncose.SigAlgFromCOSEAlg(coseAlg)

		err = aikCert.CheckSignature(x509.SignatureAlgorithm(sigAlg), certInfoBytes, sigBytes)
		if err != nil {
			return "", nil, ErrAttestationFormat.WithDetails(fmt.Sprintf("Signature validation error: %+v\n", err))
		}
		// Verify that aikCert meets the requirements in §8.3.1 TPM Attestation Statement Certificate Requirements

		// 1/6 Version MUST be set to 3.
		if aikCert.Version != 3 {
			return "", nil, ErrAttestationFormat.WithDetails("AIK certificate version must be 3")
		}
		// 2/6 Subject field MUST be set to empty.
		if aikCert.Subject.String() != "" {
			return "", nil, ErrAttestationFormat.WithDetails("AIK certificate subject must be empty")
		}

		// 3/6 The Subject Alternative Name extension MUST be set as defined in [TPMv2-EK-Profile] section 3.2.9{}
		var manufacturer, model, version string

		for _, ext := range aikCert.Extensions {
			if ext.Id.Equal([]int{2, 5, 29, 17}) {
				manufacturer, model, version, err = parseSANExtension(ext.Value)
				if err != nil {
					return "", nil, err
				}
			}
		}

		if manufacturer == "" || model == "" || version == "" {
			return "", nil, ErrAttestationFormat.WithDetails("Invalid SAN data in AIK certificate")
		}

		if !isValidTPMManufacturer(manufacturer) {
			return "", nil, ErrAttestationFormat.WithDetails("Invalid TPM manufacturer")
		}

		// 4/6 The Extended Key Usage extension MUST contain the "joint-iso-itu-t(2) internationalorganizations(23) 133 tcg-kp(8) tcg-kp-AIKCertificate(3)" OID.
		var (
			ekuValid = false
			eku      []asn1.ObjectIdentifier
		)

		for _, ext := range aikCert.Extensions {
			if ext.Id.Equal([]int{2, 5, 29, 37}) {
				rest, err := asn1.Unmarshal(ext.Value, &eku)
				if len(rest) != 0 || err != nil || !eku[0].Equal(tcgKpAIKCertificate) {
					return "", nil, ErrAttestationFormat.WithDetails("AIK certificate EKU missing 2.23.133.8.3")
				}

				ekuValid = true
			}
		}

		if !ekuValid {
			return "", nil, ErrAttestationFormat.WithDetails("AIK certificate missing EKU")
		}

		// 5/6 The Basic Constraints extension MUST have the CA component set to false.
		type basicConstraints struct {
			IsCA       bool `asn1:"optional"`
			MaxPathLen int  `asn1:"optional,default:-1"`
		}

		var constraints basicConstraints

		for _, ext := range aikCert.Extensions {
			if ext.Id.Equal([]int{2, 5, 29, 19}) {
				if rest, err := asn1.Unmarshal(ext.Value, &constraints); err != nil {
					return "", nil, ErrAttestationFormat.WithDetails("AIK certificate basic constraints malformed")
				} else if len(rest) != 0 {
					return "", nil, ErrAttestationFormat.WithDetails("AIK certificate basic constraints contains extra data")
				}
			}
		}

		// 6/6 An Authority Information Access (AIA) extension with entry id-ad-ocsp and a CRL Distribution Point
		// extension [RFC5280] are both OPTIONAL as the status of many attestation certificates is available
		// through metadata services. See, for example, the FIDO Metadata Service.
		if constraints.IsCA {
			return "", nil, ErrAttestationFormat.WithDetails("AIK certificate basic constraints missing or CA is true")
		}
	}

	return string(metadata.AttCA), x5c, err
}

func forEachSAN(extension []byte, callback func(tag int, data []byte) error) error {
	// RFC 5280, 4.2.1.6

	// SubjectAltName ::= GeneralNames
	//
	// GeneralNames ::= SEQUENCE SIZE (1..MAX) OF GeneralName
	//
	// GeneralName ::= CHOICE {
	//      otherName                       [0]     OtherName,
	//      rfc822Name                      [1]     IA5String,
	//      dNSName                         [2]     IA5String,
	//      x400Address                     [3]     ORAddress,
	//      directoryName                   [4]     Name,
	//      ediPartyName                    [5]     EDIPartyName,
	//      uniformResourceIdentifier       [6]     IA5String,
	//      iPAddress                       [7]     OCTET STRING,
	//      registeredID                    [8]     OBJECT IDENTIFIER }
	var seq asn1.RawValue

	rest, err := asn1.Unmarshal(extension, &seq)
	if err != nil {
		return err
	} else if len(rest) != 0 {
		return errors.New("x509: trailing data after X.509 extension")
	}

	if !seq.IsCompound || seq.Tag != 16 || seq.Class != 0 {
		return asn1.StructuralError{Msg: "bad SAN sequence"}
	}

	rest = seq.Bytes

	for len(rest) > 0 {
		var v asn1.RawValue

		rest, err = asn1.Unmarshal(rest, &v)
		if err != nil {
			return err
		}

		if err := callback(v.Tag, v.Bytes); err != nil {
			return err
		}
	}

	return nil
}

const (
	nameTypeDN = 4
)

var (
	tcgKpAIKCertificate  = asn1.ObjectIdentifier{2, 23, 133, 8, 3}
	tcgAtTpmManufacturer = asn1.ObjectIdentifier{2, 23, 133, 2, 1}
	tcgAtTpmModel        = asn1.ObjectIdentifier{2, 23, 133, 2, 2}
	tcgAtTpmVersion      = asn1.ObjectIdentifier{2, 23, 133, 2, 3}
)

func parseSANExtension(value []byte) (manufacturer string, model string, version string, err error) {
	err = forEachSAN(value, func(tag int, data []byte) error {
		switch tag {
		case nameTypeDN:
			tpmDeviceAttributes := pkix.RDNSequence{}
			_, err := asn1.Unmarshal(data, &tpmDeviceAttributes)
			if err != nil {
				return err
			}
			for _, rdn := range tpmDeviceAttributes {
				if len(rdn) == 0 {
					continue
				}
				for _, atv := range rdn {
					value, ok := atv.Value.(string)
					if !ok {
						continue
					}

					if atv.Type.Equal(tcgAtTpmManufacturer) {
						manufacturer = strings.TrimPrefix(value, "id:")
					}
					if atv.Type.Equal(tcgAtTpmModel) {
						model = value
					}
					if atv.Type.Equal(tcgAtTpmVersion) {
						version = strings.TrimPrefix(value, "id:")
					}
				}
			}
		}
		return nil
	})

	return
}

var tpmManufacturers = []struct {
	id   string
	name string
	code string
}{
	{"414D4400", "AMD", "AMD"},
	{"41544D4C", "Atmel", "ATML"},
	{"4252434D", "Broadcom", "BRCM"},
	{"49424d00", "IBM", "IBM"},
	{"49465800", "Infineon", "IFX"},
	{"494E5443", "Intel", "INTC"},
	{"4C454E00", "Lenovo", "LEN"},
	{"4E534D20", "National Semiconductor", "NSM"},
	{"4E545A00", "Nationz", "NTZ"},
	{"4E544300", "Nuvoton Technology", "NTC"},
	{"51434F4D", "Qualcomm", "QCOM"},
	{"534D5343", "SMSC", "SMSC"},
	{"53544D20", "ST Microelectronics", "STM"},
	{"534D534E", "Samsung", "SMSN"},
	{"534E5300", "Sinosun", "SNS"},
	{"54584E00", "Texas Instruments", "TXN"},
	{"57454300", "Winbond", "WEC"},
	{"524F4343", "Fuzhouk Rockchip", "ROCC"},
	{"FFFFF1D0", "FIDO Alliance Conformance Testing", "FIDO"},
}

func isValidTPMManufacturer(id string) bool {
	for _, m := range tpmManufacturers {
		if m.id == id {
			return true
		}
	}

	return false
}
