package draftlint

import "strings"

func smsSegments(body string) int {
	const basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	const extended = "\f^{}\\[~]|€"
	gsm, units := true, 0
	for _, r := range body {
		switch {
		case strings.ContainsRune(basic, r):
			units++
		case strings.ContainsRune(extended, r):
			units += 2
		default:
			gsm = false
		}
	}
	single, multi := 160, 153
	if !gsm {
		single, multi = 70, 67
		units = 0
		for _, r := range body {
			units++
			if r > 0xffff {
				units++
			}
		}
	}
	if units <= single {
		return 1
	}
	return (units + multi - 1) / multi
}
