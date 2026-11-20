package shumei

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"strings"
)

// AppCertHex is Last War's Android signing certificate, DER, hex-encoded: what
// android.content.pm.Signature.toCharsString() returns for the app's first
// signature. It is the one in the 1.0.370 base.apk's META-INF/BNDLTOOL.RSA
// (subject O=LCYD), which anyone can extract:
//
//	openssl pkcs7 -inform DER -in BNDLTOOL.RSA -print_certs | openssl x509 -outform DER | xxd -p
//
// The fingerprint's a56/a57 describe it (appCertSubject, appCertHash), and the
// Android Login's psh hashes the same hex, so one certificate change updates all
// three.
const AppCertHex = "3082029c30820184a00302010202046fdfb4c4300d06092a864886f70d0101050500300f310d300b060355040a0c044c4359443020170d3233303131363133323131335a180f32303733303130333133323131335a300f310d300b060355040a0c044c43594430820122300d06092a864886f70d01010105000382010f003082010a028201010089ef3aa54cf5a75c83d18c37694482552227620de97dbce3747d0d820b94400a322a65ad61b8360deaf036a9755a4058a6eb01388f6fcb3b061bea3730bc401022471739ebcbe3337c3a67e211c5f7a909397198a6824a0f71a9d8c715575b6529055d922fe7e007ce71b16098c491bdea6f4767ac2e2e7e6cbaa5fbb11c55ffb6561a895c3e7437cb41697db019005473d9f77f4430b163848231ec4903259949b489d5eb436f9cbf4206fcbf040d8c6da6300e30edfeecb5619abfb79dbab2837b4adddba51582a5c85e50568ae32355eba1f974f5567dbb4ac8afdd9ce474f6555e9f5f1085b82072052a08a772db1ee943615b38833dd1e95f0c827ffeed0203010001300d06092a864886f70d01010505000382010100488695eee865bc3992e9dcfa86a19719eb753b7abcd7a5d7ddfd9471229136b37bf08bce8520a98280c0915202ea5a6530f12f81fc6b85769685d8049e73f94d7584ad5e62babc1f9c8c34617f2856d40f7592dad213a0b29edcb5c6ef9179e2d220fd4d83ed42bd6b4f0e430bbcb970db89b352aac9a41f90b15dcb1f76f3efd8e6d104fc23c71c77d50448d9b7666ca182210a33d95b7786a2400f20ff6b9bb33a464bd1134f14eb6a4d6c169d526c2875a5ba0dca2f1f8b8920d1eb3dabfa7977f35fd348eba2a72ee7b052dd8618d5a50b84c25a5e7afdeba271c5eff310d49b934bd8b80724dd7784f35550cfe72c6e0a00140bf50f45acbe1a7eafc2ff"

// appCertSubject and appCertHash are a56 and a57: the subject DN of the app's
// own first signing certificate and android.content.pm.Signature.hashCode() of
// it (l111l1111llIl.java:395-397; l111l11111lIl.java:148-153, 264-274, 413-420
// read getPackageInfo(own package, GET_SIGNATURES).signatures[0]). They belong
// to the app, not to the phone, so every catalog entry reports the same pair.
var appCertSubject, appCertHash = appCertFields(AppCertHex)

// appCertFields returns a56 and a57 for the hex DER certificate. It panics on a
// certificate that doesn't parse: AppCertHex is a constant, and a test pins it.
func appCertFields(certHex string) (string, int32) {
	der, err := hex.DecodeString(certHex)
	if err != nil {
		panic("shumei: AppCertHex is not hex: " + err.Error())
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic("shumei: AppCertHex is not an X.509 certificate: " + err.Error())
	}
	return javaSubjectDN(cert.RawSubject), javaBytesHashCode(der)
}

// javaSubjectDN formats a DER Name the way X509Certificate.getSubjectDN()
// .toString() does: the RDNs most-specific first, joined with ", "
// ("CN=Android, OU=Android, O=Google Inc., ..."). Go's pkix.Name.String() uses
// the same order but no space, so each RDN is formatted on its own.
func javaSubjectDN(rawSubject []byte) string {
	var seq pkix.RDNSequence
	if _, err := asn1.Unmarshal(rawSubject, &seq); err != nil {
		return ""
	}
	parts := make([]string, 0, len(seq))
	for i := len(seq) - 1; i >= 0; i-- {
		parts = append(parts, pkix.RDNSequence{seq[i]}.String())
	}
	return strings.Join(parts, ", ")
}

// javaBytesHashCode is java.util.Arrays.hashCode(byte[]), which
// Signature.hashCode() returns for the certificate's bytes: 31*h + b over the
// signed bytes, starting from 1, in int arithmetic.
func javaBytesHashCode(b []byte) int32 {
	h := int32(1)
	for _, c := range b {
		h = 31*h + int32(int8(c))
	}
	return h
}
