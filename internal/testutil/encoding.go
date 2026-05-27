package testutil

// EncodedSamplesT holds encoded byte sequences for testing charset detection and repair.
// Field names intentionally use underscores to separate the encoding name from
// the sample content, which is far more readable than camelCase.
//
//nolint:revive // underscore-separated encoding+sample names are intentional
type EncodedSamplesT struct {
	ShiftJIS_Konnichiwa     []byte
	GBK_Nihao               []byte
	Big5_Nihao              []byte
	EUCKR_Annyeong          []byte
	Win1252_SmartQuoteRight []byte
	Win1252_EnDash          []byte
	Win1252_EmDash          []byte
	Win1252_DoubleQuotes    []byte
	Win1252_Trademark       []byte
	Win1252_Bullet          []byte
	Win1252_Euro            []byte
	Latin1_OAcute           []byte
	Latin1_CCedilla         []byte
	Latin1_UUmlaut          []byte
	Latin1_NTilde           []byte
	Latin1_Registered       []byte
	Latin1_Degree           []byte

	// Longer Asian encoding samples for reliable charset detection.
	// These are long enough for chardet to identify with high confidence.
	ShiftJIS_Long      []byte
	ShiftJIS_Long_UTF8 string
	GBK_Long           []byte
	GBK_Long_UTF8      string
	Big5_Long          []byte
	Big5_Long_UTF8     string
	EUCKR_Long         []byte
	EUCKR_Long_UTF8    string
}

// encodedSamples holds the canonical byte sequences (unexported to prevent direct mutation).
var encodedSamples = EncodedSamplesT{
	ShiftJIS_Konnichiwa: []byte{0x82, 0xb1, 0x82, 0xf1, 0x82, 0xc9, 0x82, 0xbf, 0x82, 0xcd},
	GBK_Nihao:           []byte{0xc4, 0xe3, 0xba, 0xc3},
	Big5_Nihao:          []byte{0xa9, 0x6f, 0xa6, 0x6e},
	EUCKR_Annyeong:      []byte{0xbe, 0xc8, 0xb3, 0xe7},

	Win1252_SmartQuoteRight: []byte("Rand\x92s Opponent"),
	Win1252_EnDash:          []byte("2020 \x96 2024"),
	Win1252_EmDash:          []byte("Hello\x97World"),
	Win1252_DoubleQuotes:    []byte("\x93Hello\x94"),
	Win1252_Trademark:       []byte("Brand\x99"),
	Win1252_Bullet:          []byte("\x95 Item"),
	Win1252_Euro:            []byte("Price: \x80100"),

	Latin1_OAcute:     []byte("Mir\xf3 - Picasso"),
	Latin1_CCedilla:   []byte("Gar\xe7on"),
	Latin1_UUmlaut:    []byte("M\xfcnchen"),
	Latin1_NTilde:     []byte("Espa\xf1a"),
	Latin1_Registered: []byte("Laguiole.com \xae"),
	Latin1_Degree:     []byte("25\xb0C"),

	// Shift-JIS: "日本語のテキストサンプルです。これは文字化けのテストに使用されます。"
	// (Japanese text sample. This is used for character corruption testing.)
	ShiftJIS_Long: []byte{
		0x93, 0xfa, 0x96, 0x7b, 0x8c, 0xea, 0x82, 0xcc, 0x83, 0x65, 0x83, 0x4c,
		0x83, 0x58, 0x83, 0x67, 0x83, 0x54, 0x83, 0x93, 0x83, 0x76, 0x83, 0x8b,
		0x82, 0xc5, 0x82, 0xb7, 0x81, 0x42, 0x82, 0xb1, 0x82, 0xea, 0x82, 0xcd,
		0x95, 0xb6, 0x8e, 0x9a, 0x89, 0xbb, 0x82, 0xaf, 0x82, 0xcc, 0x83, 0x65,
		0x83, 0x58, 0x83, 0x67, 0x82, 0xc9, 0x8e, 0x67, 0x97, 0x70, 0x82, 0xb3,
		0x82, 0xea, 0x82, 0xdc, 0x82, 0xb7, 0x81, 0x42,
	},
	ShiftJIS_Long_UTF8: "日本語のテキストサンプルです。これは文字化けのテストに使用されます。",

	// GBK: "这是一个中文文本示例，用于测试字符编码检测功能。"
	// (This is a Chinese text sample for testing character encoding detection.)
	GBK_Long: []byte{
		0xd5, 0xe2, 0xca, 0xc7, 0xd2, 0xbb, 0xb8, 0xf6, 0xd6, 0xd0, 0xce, 0xc4,
		0xce, 0xc4, 0xb1, 0xbe, 0xca, 0xbe, 0xc0, 0xfd, 0xa3, 0xac, 0xd3, 0xc3,
		0xd3, 0xda, 0xb2, 0xe2, 0xca, 0xd4, 0xd7, 0xd6, 0xb7, 0xfb, 0xb1, 0xe0,
		0xc2, 0xeb, 0xbc, 0xec, 0xb2, 0xe2, 0xb9, 0xa6, 0xc4, 0xdc, 0xa1, 0xa3,
	},
	GBK_Long_UTF8: "这是一个中文文本示例，用于测试字符编码检测功能。",

	// Big5: "這是一個繁體中文範例，用於測試字元編碼偵測。"
	// (This is a Traditional Chinese sample for testing character encoding detection.)
	Big5_Long: []byte{
		0xb3, 0x6f, 0xac, 0x4f, 0xa4, 0x40, 0xad, 0xd3, 0xc1, 0x63, 0xc5, 0xe9,
		0xa4, 0xa4, 0xa4, 0xe5, 0xbd, 0x64, 0xa8, 0xd2, 0xa1, 0x41, 0xa5, 0xce,
		0xa9, 0xf3, 0xb4, 0xfa, 0xb8, 0xd5, 0xa6, 0x72, 0xa4, 0xb8, 0xbd, 0x73,
		0xbd, 0x58, 0xb0, 0xbb, 0xb4, 0xfa, 0xa1, 0x43,
	},
	Big5_Long_UTF8: "這是一個繁體中文範例，用於測試字元編碼偵測。",

	// EUC-KR: "한글 텍스트 샘플입니다. 인코딩 감지 테스트용입니다."
	// (Korean text sample. For encoding detection testing.)
	EUCKR_Long: []byte{
		0xc7, 0xd1, 0xb1, 0xdb, 0x20, 0xc5, 0xd8, 0xbd, 0xba, 0xc6, 0xae, 0x20,
		0xbb, 0xf9, 0xc7, 0xc3, 0xc0, 0xd4, 0xb4, 0xcf, 0xb4, 0xd9, 0x2e, 0x20,
		0xc0, 0xce, 0xc4, 0xda, 0xb5, 0xf9, 0x20, 0xb0, 0xa8, 0xc1, 0xf6, 0x20,
		0xc5, 0xd7, 0xbd, 0xba, 0xc6, 0xae, 0xbf, 0xeb, 0xc0, 0xd4, 0xb4, 0xcf,
		0xb4, 0xd9, 0x2e,
	},
	EUCKR_Long_UTF8: "한글 텍스트 샘플입니다. 인코딩 감지 테스트용입니다.",
}

func cloneBytes(b []byte) []byte {
	return append([]byte(nil), b...)
}

// EncodedSamples returns a fresh copy of all encoded byte samples, safe for
// mutation by individual tests without cross-test coupling.
//
// MAINTAINER NOTE: This function uses explicit field copying rather than
// reflection. This is intentional. Reflection-based "automatic" copying:
// - Adds complexity (handling unexported fields, nil slices, etc.)
// - Requires extensive test coverage for the reflection code itself
// - Is not worth it for a test helper with infrequent field additions
//
// If you add a new field to EncodedSamplesT, add a corresponding line here.
// TestEncodedSamplesNonEmpty will catch any missing []byte fields.
func EncodedSamples() EncodedSamplesT {
	return EncodedSamplesT{
		ShiftJIS_Konnichiwa:     cloneBytes(encodedSamples.ShiftJIS_Konnichiwa),
		GBK_Nihao:               cloneBytes(encodedSamples.GBK_Nihao),
		Big5_Nihao:              cloneBytes(encodedSamples.Big5_Nihao),
		EUCKR_Annyeong:          cloneBytes(encodedSamples.EUCKR_Annyeong),
		Win1252_SmartQuoteRight: cloneBytes(encodedSamples.Win1252_SmartQuoteRight),
		Win1252_EnDash:          cloneBytes(encodedSamples.Win1252_EnDash),
		Win1252_EmDash:          cloneBytes(encodedSamples.Win1252_EmDash),
		Win1252_DoubleQuotes:    cloneBytes(encodedSamples.Win1252_DoubleQuotes),
		Win1252_Trademark:       cloneBytes(encodedSamples.Win1252_Trademark),
		Win1252_Bullet:          cloneBytes(encodedSamples.Win1252_Bullet),
		Win1252_Euro:            cloneBytes(encodedSamples.Win1252_Euro),
		Latin1_OAcute:           cloneBytes(encodedSamples.Latin1_OAcute),
		Latin1_CCedilla:         cloneBytes(encodedSamples.Latin1_CCedilla),
		Latin1_UUmlaut:          cloneBytes(encodedSamples.Latin1_UUmlaut),
		Latin1_NTilde:           cloneBytes(encodedSamples.Latin1_NTilde),
		Latin1_Registered:       cloneBytes(encodedSamples.Latin1_Registered),
		Latin1_Degree:           cloneBytes(encodedSamples.Latin1_Degree),
		ShiftJIS_Long:           cloneBytes(encodedSamples.ShiftJIS_Long),
		ShiftJIS_Long_UTF8:      encodedSamples.ShiftJIS_Long_UTF8,
		GBK_Long:                cloneBytes(encodedSamples.GBK_Long),
		GBK_Long_UTF8:           encodedSamples.GBK_Long_UTF8,
		Big5_Long:               cloneBytes(encodedSamples.Big5_Long),
		Big5_Long_UTF8:          encodedSamples.Big5_Long_UTF8,
		EUCKR_Long:              cloneBytes(encodedSamples.EUCKR_Long),
		EUCKR_Long_UTF8:         encodedSamples.EUCKR_Long_UTF8,
	}
}
