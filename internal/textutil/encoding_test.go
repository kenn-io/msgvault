package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestEnsureUTF8_AlreadyValid(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{"ASCII", []byte("Hello, World!"), "Hello, World!"},
		{"UTF-8 Chinese", []byte("你好世界"), "你好世界"},
		{"UTF-8 Japanese", []byte("こんにちは"), "こんにちは"},
		{"UTF-8 Korean", []byte("안녕하세요"), "안녕하세요"},
		{"UTF-8 Cyrillic", []byte("Привет мир"), "Привет мир"},
		{"UTF-8 mixed", []byte("Hello 世界! Привет!"), "Hello 世界! Привет!"},
		{"UTF-8 emoji", []byte("Hello 👋 World 🌍"), "Hello 👋 World 🌍"},
		{"empty string", []byte(""), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EnsureUTF8(string(tt.input))
			assertpkg.Equal(t, tt.expected, result)
			assertpkg.True(t, utf8.ValidString(result), "result is not valid UTF-8: %q", result)
		})
	}
}

func TestEnsureUTF8_Windows1252(t *testing.T) {
	enc := testutil.EncodedSamples()
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{"smart single quote (right)", enc.Win1252_SmartQuoteRight, "Rand\u2019s Opponent"},
		{"en dash", enc.Win1252_EnDash, "2020 \u2013 2024"},
		{"em dash", enc.Win1252_EmDash, "Hello\u2014World"},
		{"left double quote", enc.Win1252_DoubleQuotes, "\u201cHello\u201d"},
		{"trademark", enc.Win1252_Trademark, "Brand\u2122"},
		{"bullet", enc.Win1252_Bullet, "\u2022 Item"},
		{"euro sign", enc.Win1252_Euro, "Price: \u20ac100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EnsureUTF8(string(tt.input))
			assertpkg.Equal(t, tt.expected, result)
			assertpkg.True(t, utf8.ValidString(result), "result is not valid UTF-8: %q", result)
		})
	}
}

func TestEnsureUTF8_Latin1(t *testing.T) {
	enc := testutil.EncodedSamples()
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{"o with acute", enc.Latin1_OAcute, "Miró - Picasso"},
		{"c with cedilla", enc.Latin1_CCedilla, "Garçon"},
		{"u with umlaut", enc.Latin1_UUmlaut, "München"},
		{"n with tilde", enc.Latin1_NTilde, "España"},
		{"registered trademark", enc.Latin1_Registered, "Laguiole.com ®"},
		{"degree symbol", enc.Latin1_Degree, "25°C"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EnsureUTF8(string(tt.input))
			assertpkg.Equal(t, tt.expected, result)
			assertpkg.True(t, utf8.ValidString(result), "result is not valid UTF-8: %q", result)
		})
	}
}

func TestEnsureUTF8_AsianEncodings(t *testing.T) {
	// Test that EnsureUTF8 produces valid UTF-8 from Asian-encoded input.
	// We don't assert exact decoded strings because chardet heuristics may vary
	// across library versions. Instead, we verify:
	// 1. Output is valid UTF-8
	// 2. Output is non-empty
	// 3. Output doesn't contain replacement characters (successful decode)
	// 4. Output contains stable substrings from the expected decoded text
	enc := testutil.EncodedSamples()
	tests := []struct {
		name             string
		input            []byte
		expectedContains []string // Stable substrings that must appear in correct decode
	}{
		// Japanese: "日本語のテキストサンプルです。これは文字化けのテストに使用されます。"
		// Check for key characters that wouldn't appear in a wrong decode
		{"Shift-JIS Japanese", enc.ShiftJIS_Long, []string{"日本語", "テキスト", "です"}},
		// Chinese (Simplified): "这是一个中文文本示例，用于测试字符编码检测功能。"
		{"GBK Simplified Chinese", enc.GBK_Long, []string{"中文", "测试", "编码"}},
		// Chinese (Traditional): "這是一個繁體中文範例，用於測試字元編碼偵測。"
		{"Big5 Traditional Chinese", enc.Big5_Long, []string{"繁體中文", "測試", "編碼"}},
		// Korean: "한글 텍스트 샘플입니다. 인코딩 감지 테스트용입니다."
		{"EUC-KR Korean", enc.EUCKR_Long, []string{"한글", "텍스트", "인코딩"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			result := EnsureUTF8(string(tt.input))
			assert.True(utf8.ValidString(result), "result is not valid UTF-8: %q", result)
			assert.NotEmpty(result, "result is empty")
			// Verify no replacement characters (indicates failed decode)
			assert.False(strings.ContainsRune(result, '\ufffd'),
				"result contains replacement character, suggesting decode failure: %q", result)
			// Verify correctness: output must contain expected substrings
			for _, substr := range tt.expectedContains {
				assert.Containsf(result, substr, "result missing expected substring")
			}
		})
	}
}

func TestEnsureUTF8_MixedContent(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		contains []string
	}{
		{
			"email subject with smart quotes",
			[]byte("Re: Can\x92t access the \x93dashboard\x94"),
			[]string{"Re:", "Can", "access the", "dashboard"},
		},
		{
			"price with currency",
			[]byte("Only \x80199.99 \x96 Limited Time"),
			[]string{"Only", "199.99", "Limited Time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EnsureUTF8(string(tt.input))
			assertpkg.True(t, utf8.ValidString(result), "result is not valid UTF-8: %q", result)
			for _, sub := range tt.contains {
				assertpkg.Contains(t, result, sub)
			}
		})
	}
}

func TestSanitizeUTF8(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"valid UTF-8 unchanged", "Hello, 世界!", "Hello, 世界!"},
		{"single invalid byte", "Hello\x80World", "Hello\ufffdWorld"},
		{"multiple invalid bytes", "Test\x80\x81\x82String", "Test\ufffd\ufffd\ufffdString"},
		{"truncated UTF-8 sequence", "Hello\xc3", "Hello\ufffd"},
		{"invalid continuation byte", "Test\xc3\x00End", "Test\ufffd\x00End"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeUTF8(tt.input)
			assertpkg.Equalf(t, tt.expected, result, "SanitizeUTF8(%q)", tt.input)
			assertpkg.True(t, utf8.ValidString(result), "result is not valid UTF-8: %q", result)
		})
	}
}

func TestGetEncodingByName(t *testing.T) {
	tests := []struct {
		charset    string
		wantNil    bool
		verifyByte byte // A byte that decodes differently in this charset vs ASCII
		wantRune   rune // Expected rune when decoding verifyByte
	}{
		// Windows-1252: 0x92 = right single quote (')
		{"windows-1252", false, 0x92, '\u2019'},
		{"CP1252", false, 0x92, '\u2019'},
		// ISO-8859-1: 0xE9 = é
		{"ISO-8859-1", false, 0xe9, 'é'},
		{"iso-8859-1", false, 0xe9, 'é'},
		{"latin1", false, 0xe9, 'é'},
		// Shift_JIS: two-byte sequence 0x82 0xA0 = あ (hiragana a)
		{"Shift_JIS", false, 0, 0}, // Skip byte verification for multi-byte
		{"shift_jis", false, 0, 0},
		// Other encodings - verify non-nil only
		{"EUC-JP", false, 0, 0},
		{"EUC-KR", false, 0, 0},
		{"GBK", false, 0, 0},
		{"GB2312", false, 0, 0},
		{"Big5", false, 0, 0},
		{"KOI8-R", false, 0, 0},
		// Unknown charset should return nil
		{"unknown-charset", true, 0, 0},
		{"", true, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.charset, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			enc := GetEncodingByName(tt.charset)
			if tt.wantNil {
				assert.Nilf(enc, "GetEncodingByName(%q)", tt.charset)
				return
			}
			require.NotNilf(enc, "GetEncodingByName(%q) = nil, want encoding", tt.charset)
			// Verify encoding identity by decoding a characteristic byte
			if tt.verifyByte != 0 {
				decoded, err := enc.NewDecoder().Bytes([]byte{tt.verifyByte})
				require.NoError(err, "decode failed")
				got := []rune(string(decoded))
				if assert.Lenf(got, 1, "decoding 0x%02x: got %q, want %q", tt.verifyByte, string(got), string(tt.wantRune)) {
					assert.Equalf(tt.wantRune, got[0],
						"decoding 0x%02x: got %q, want %q", tt.verifyByte, string(got), string(tt.wantRune))
				}
			}
		})
	}
}

func TestGetEncodingByName_DecodesCorrectly(t *testing.T) {
	// Verify that GetEncodingByName returns encodings that decode test samples correctly.
	enc := testutil.EncodedSamples()
	tests := []struct {
		name     string
		charset  string
		input    []byte
		expected string
	}{
		{"Shift-JIS", "Shift_JIS", enc.ShiftJIS_Long, enc.ShiftJIS_Long_UTF8},
		{"GBK", "GBK", enc.GBK_Long, enc.GBK_Long_UTF8},
		{"Big5", "Big5", enc.Big5_Long, enc.Big5_Long_UTF8},
		{"EUC-KR", "EUC-KR", enc.EUCKR_Long, enc.EUCKR_Long_UTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoding := GetEncodingByName(tt.charset)
			requirepkg.NotNilf(t, encoding, "GetEncodingByName(%q) returned nil", tt.charset)
			decoded, err := encoding.NewDecoder().Bytes(tt.input)
			requirepkg.NoError(t, err, "decode failed")
			assertpkg.Equal(t, tt.expected, string(decoded))
		})
	}
}

func TestGetEncodingByName_MatchesExpectedEncodings(t *testing.T) {
	// Verify that charset names map to the correct encoding objects.
	tests := []struct {
		charset  string
		wantName string // Use a test decoding to verify identity
	}{
		// Test that similar charset names return the same encoding
		{"windows-1252", "windows-1252"},
		{"CP1252", "windows-1252"},
		{"cp1252", "windows-1252"},
		{"ISO-8859-1", "iso-8859-1"},
		{"iso-8859-1", "iso-8859-1"},
		{"latin1", "iso-8859-1"},
		{"latin-1", "iso-8859-1"},
	}
	for _, tt := range tests {
		t.Run(tt.charset, func(t *testing.T) {
			require := requirepkg.New(t)
			enc := GetEncodingByName(tt.charset)
			expected := GetEncodingByName(tt.wantName)
			require.NotNil(enc, "encoding is nil")
			require.NotNil(expected, "expected encoding is nil")
			// Verify they decode the same way
			testBytes := []byte{0x80, 0x92, 0xe9, 0xf1}
			got, err := enc.NewDecoder().Bytes(testBytes)
			require.NoErrorf(err, "decoder error for %q", tt.charset)
			want, err := expected.NewDecoder().Bytes(testBytes)
			require.NoErrorf(err, "decoder error for %q", tt.wantName)
			assertpkg.Equalf(t, string(want), string(got),
				"%q and %q decode differently", tt.charset, tt.wantName)
		})
	}
}

func TestEncodingIdentity(t *testing.T) {
	// Verify that GetEncodingByName returns the correct encoding type
	// by checking that decoding produces expected results for each encoding.
	tests := []struct {
		name     string
		charset  string
		input    []byte
		expected string
	}{
		{
			"Shift_JIS hiragana",
			"Shift_JIS",
			[]byte{0x82, 0xa0, 0x82, 0xa2, 0x82, 0xa4}, // あいう
			"あいう",
		},
		{
			"EUC-JP hiragana",
			"EUC-JP",
			[]byte{0xa4, 0xa2, 0xa4, 0xa4, 0xa4, 0xa6}, // あいう
			"あいう",
		},
		{
			"GBK chinese",
			"GBK",
			[]byte{0xc4, 0xe3, 0xba, 0xc3}, // 你好
			"你好",
		},
		{
			"Big5 chinese",
			"Big5",
			[]byte{0xa7, 0x41, 0xa6, 0x6e}, // 你好
			"你好",
		},
		{
			"EUC-KR korean",
			"EUC-KR",
			[]byte{0xbe, 0xc8, 0xb3, 0xe7}, // 안녕
			"안녕",
		},
		{
			"KOI8-R cyrillic",
			"KOI8-R",
			[]byte{0xf0, 0xf2, 0xe9, 0xf7, 0xe5, 0xf4}, // ПРИВЕТ
			"ПРИВЕТ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := GetEncodingByName(tt.charset)
			requirepkg.NotNilf(t, enc, "GetEncodingByName(%q) returned nil", tt.charset)
			decoded, err := enc.NewDecoder().Bytes(tt.input)
			requirepkg.NoError(t, err, "decode error")
			assertpkg.Equal(t, tt.expected, string(decoded))
		})
	}
}

func TestGetEncodingByName_ReturnsCorrectType(t *testing.T) {
	// Verify that specific charset names return encodings that decode identically
	// to the expected encoding types. Uses behavior-based comparison with multiple
	// discriminating byte sequences to distinguish closely related encodings.
	tests := []struct {
		charset  string
		expected encoding.Encoding
		inputs   [][]byte // Multiple byte sequences to better distinguish encodings
	}{
		// Shift_JIS: Use multiple sequences including half-width katakana (0xA1-0xDF)
		// which is handled differently in Shift_JIS vs some variants
		{"Shift_JIS", japanese.ShiftJIS, [][]byte{
			{0x82, 0xa0, 0x82, 0xa2}, // あい (hiragana)
			{0x83, 0x41, 0x83, 0x42}, // アイ (full-width katakana)
			{0xb1, 0xb2, 0xb3},       // ｱｲｳ (half-width katakana)
			{0x93, 0xfa, 0x96, 0x7b}, // 日本
		}},
		// EUC-JP: Uses different byte ranges than Shift_JIS
		{"EUC-JP", japanese.EUCJP, [][]byte{
			{0xa4, 0xa2, 0xa4, 0xa4},             // あい
			{0xa5, 0xa2, 0xa5, 0xa4},             // アイ
			{0x8e, 0xb1, 0x8e, 0xb2, 0x8e, 0xb3}, // ｱｲｳ (half-width via SS2)
			{0xc6, 0xfc, 0xcb, 0xdc},             // 日本
		}},
		// EUC-KR: Korean-specific sequences
		{"EUC-KR", korean.EUCKR, [][]byte{
			{0xbe, 0xc8, 0xb3, 0xe7}, // 안녕
			{0xc7, 0xd1, 0xb1, 0xdb}, // 한글
			{0xb0, 0xa1, 0xb0, 0xa2}, // 가각 (common jamo combinations)
		}},
		// GBK: Simplified Chinese sequences with GB2312 subset and GBK extensions
		{"GBK", simplifiedchinese.GBK, [][]byte{
			{0xc4, 0xe3, 0xba, 0xc3}, // 你好
			{0xd6, 0xd0, 0xce, 0xc4}, // 中文
			{0x81, 0x40},             // GBK extension character (丂)
		}},
		// Big5: Traditional Chinese sequences
		{"Big5", traditionalchinese.Big5, [][]byte{
			{0xa7, 0x41, 0xa6, 0x6e}, // 你好
			{0xa4, 0xa4, 0xa4, 0xe5}, // 中文
			{0xa1, 0x40},             // ideographic space
		}},
	}
	for _, tt := range tests {
		t.Run(tt.charset, func(t *testing.T) {
			require := requirepkg.New(t)
			enc := GetEncodingByName(tt.charset)
			require.NotNilf(enc, "GetEncodingByName(%q) returned nil", tt.charset)
			for i, input := range tt.inputs {
				got, err := enc.NewDecoder().Bytes(input)
				require.NoErrorf(err, "decoder error on input[%d] %x", i, input)
				want, err := tt.expected.NewDecoder().Bytes(input)
				require.NoErrorf(err, "expected decoder error on input[%d] %x", i, input)
				assertpkg.Equalf(t, string(want), string(got),
					"GetEncodingByName(%q) decodes input[%d] %x differently", tt.charset, i, input)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxRunes int
		expected string
	}{
		{"short ASCII", "Hello", 10, "Hello"},
		{"exact length", "Hello", 5, "Hello"},
		{"truncate ASCII", "Hello World", 8, "Hello..."},
		{"empty string", "", 5, ""},
		{"max 3", "Hello", 3, "Hel"},
		{"max 4", "Hello", 4, "H..."},
		{"UTF-8 no truncate", "你好世界", 4, "你好世界"}, // 4 runes, no truncation needed
		{"UTF-8 truncate", "你好世界！", 4, "你..."},
		{"emoji", "Hello 👋 World", 9, "Hello ..."},
		{"max 0", "Hello", 0, ""},
		{"max negative", "Hello", -1, ""},
		{"max 1", "Hello", 1, "H"},
		{"max 2", "Hello", 2, "He"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TruncateRunes(tt.input, tt.maxRunes)
			assertpkg.Equalf(t, tt.expected, result, "TruncateRunes(%q, %d)", tt.input, tt.maxRunes)
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"single line", "Hello World", "Hello World"},
		{"multi line", "First\nSecond\nThird", "First"},
		{"empty string", "", ""},
		{"trailing newline", "Hello\n", "Hello"},
		{"only newline", "\n", ""},
		{"leading newline", "\nSecond\nThird", "Second"},
		{"multiple leading newlines", "\n\n\nFourth", "Fourth"},
		{"leading carriage return", "\r\nSecond", "Second"},
		{"mixed leading newlines", "\r\n\n\rThird", "Third"},
		{"only newlines", "\n\n\n", ""},
		{"long line truncated", strings.Repeat("x", 250), strings.Repeat("x", 197) + "..."},
		{"exactly 200 runes", strings.Repeat("y", 200), strings.Repeat("y", 200)},
		{"long first line of multi", strings.Repeat("z", 250) + "\nSecond", strings.Repeat("z", 197) + "..."},
		{"unicode truncation safe", strings.Repeat("é", 250), strings.Repeat("é", 197) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FirstLine(tt.input)
			assertpkg.Equalf(t, tt.expected, result, "FirstLine(%q)", tt.input)
		})
	}
}

func TestSanitizeTerminal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "Hello World", "Hello World"},
		{"preserves tabs", "col1\tcol2", "col1\tcol2"},
		{"replaces newlines with spaces", "line1\nline2", "line1 line2"},
		{"replaces CR with space", "over\rwrite", "over write"},
		{"strips CSI color", "\x1b[31mred\x1b[0m", "red"},
		{"strips CSI cursor move", "\x1b[2Ahello", "hello"},
		{"strips OSC title (BEL)", "\x1b]0;evil title\x07safe", "safe"},
		{"strips OSC title (ST)", "\x1b]0;evil\x1b\\safe", "safe"},
		{"strips BEL", "\x07beep", "beep"},
		{"strips null bytes", "a\x00b", "ab"},
		{"strips C1 control byte", "a\x8fb", "ab"},
		{"strips UTF-8 encoded C1 CSI (U+009B)", "a\xc2\x9bb", "ab"},
		{"strips UTF-8 encoded C1 0x80-0x9F range", "a\xc2\x80z\xc2\x9fb", "azb"},
		{"preserves unicode", "café ☕ 日本語", "café ☕ 日本語"},
		{"strips embedded ESC seq", "before\x1b[1;32mgreen\x1b[0mafter", "beforegreenafter"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeTerminal(tt.input)
			assertpkg.Equalf(t, tt.want, got, "SanitizeTerminal(%q)", tt.input)
		})
	}
}
