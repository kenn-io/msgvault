package msgsegment

import "strings"

var attributions = [][2]string{
	{"on ", "wrote:"}, {"am ", "schrieb"}, {"le ", "a écrit"}, {"el ", "escribió"},
	{"il ", "ha scritto"}, {"em ", "escreveu"}, {"op ", "schreef"}, {"den ", "skrev"},
	{"", "来信"}, {"", "書きました"}, {"", "写道"}, {"", "написал"},
}

func attributionStart(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, m := range attributions {
		if m[0] != "" && strings.HasPrefix(s, m[0]) {
			return true
		}
	}
	return false
}

// A wrapped header needs a visible boundary in its first line. A dated
// authored sentence followed by a speaker line is not enough evidence.
func wrappedAttributionStart(s string) bool {
	s = strings.TrimSpace(s)
	return attributionStart(s) && (strings.HasSuffix(s, ",") || strings.HasSuffix(s, "<") || strings.HasSuffix(s, ">"))
}

func attributionLine(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasSuffix(s, ":") && !strings.HasSuffix(s, "：") {
		return false
	}
	for _, m := range attributions {
		if strings.HasPrefix(s, m[0]) && strings.Contains(s, m[1]) {
			return true
		}
	}
	return false
}
func forwardMarker(s string) bool {
	s = strings.ToLower(s)
	for _, m := range []string{"forwarded message", "begin forwarded message:", "weitergeleitete nachricht", "message transféré", "message transmis", "mensaje reenviado", "messaggio inoltrato", "mensagem encaminhada", "doorgestuurd bericht", "転送メッセージ", "转发的邮件", "пересланное сообщение"} {
		marker := strings.Trim(s, "- _\t:")
		if marker == strings.Trim(m, ":") {
			return true
		}
	}
	return false
}
func originalMarker(s string) bool {
	s = strings.ToLower(strings.Trim(s, "- _\t"))
	return s == "original message" || s == "ursprüngliche nachricht" || s == "message d'origine"
}
func forwardSubject(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"fwd:", "fw:", "wg:", "tr:", "rv:", "i:", "enc:", "vs:"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
func header(s string) (string, string) {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return "", ""
	}
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "from", "von", "de", "da":
		return "from", strings.TrimSpace(v)
	case "date", "sent", "gesendet", "envoyé", "enviado", "inviato":
		return "date", strings.TrimSpace(v)
	case "subject", "betreff", "objet", "asunto", "oggetto":
		return "subject", strings.TrimSpace(v)
	case "to", "an", "à", "para", "a":
		return "to", strings.TrimSpace(v)
	case "cc":
		return "cc", strings.TrimSpace(v)
	}
	return "", ""
}
func headerBlock(lines []line, i int) bool {
	from, other := false, false
	for j := i; j < min(i+4, len(lines)); j++ {
		k, _ := header(lines[j].text)
		if k == "" {
			break
		}
		from = from || k == "from"
		other = other || k == "date" || k == "subject"
	}
	return from && other
}
func claimedHeaders(lines []line, i int) *Claimed {
	c := &Claimed{ContentTrust: "untrusted_text"}
	for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
		i++
	}
	for j := i; j < min(i+12, len(lines)); j++ {
		k, v := header(lines[j].text)
		if k == "" {
			break
		}
		switch k {
		case "from":
			if c.From == "" {
				c.From = v
			}
		case "date":
			if c.Date == "" {
				c.Date = v
			}
		case "subject":
			if c.Subject == "" {
				c.Subject = v
			}
		case "to":
			if c.To == "" {
				c.To = v
			}
		}
	}
	return c
}
