package main

import (
	"regexp"
	"strings"
)

// Conversions between common Markdown (OpenBSP's native text format) and
// WhatsApp's formatting flavor, mirroring open-bsp-api's
// _shared/markdown.ts. A marker only counts as formatting when it would
// actually render: non-space text on the inside, non-alphanumeric bounded
// on the outside, single line. Anything else (a_b.pdf, 2*3) is literal and
// must survive untouched.
//
// WhatsApp `_italic_` is intentionally NOT converted: word-bounded
// `_italic_` already renders as italic in common Markdown, and intraword
// underscores (snake_case) are literal on both sides.

var (
	codeBlockRe  = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRe = regexp.MustCompile("`[^`\n]+`")

	// (boundary) * (non-space ... non-space, same line, no *) * (boundary)
	// Go's RE2 has no lookarounds, so the trailing boundary is consumed and
	// re-inserted; the fixpoint loop in convertMarker catches markers whose
	// leading boundary was consumed by the previous match ("*a* *b*").
	waBoldRe   = regexp.MustCompile(`(^|[^\pL\pN*])\*([^\s*](?:[^*\n]*?[^\s*])??)\*($|[^\pL\pN*])`)
	waStrikeRe = regexp.MustCompile(`(^|[^\pL\pN~])~([^\s~](?:[^~\n]*?[^\s~])??)~($|[^\pL\pN~])`)

	mdHeaderRe = regexp.MustCompile(`(?m)^#+\s+(.*)$`)
	mdBoldRe   = regexp.MustCompile(`\*\*([^\s*](?:.*?[^\s*])??)\*\*`)
	mdBoldUsRe = regexp.MustCompile(`__([^\s_](?:.*?[^\s_])??)__`)
	mdStrikeRe = regexp.MustCompile(`~~([^\s~](?:.*?[^\s~])??)~~`)
)

const boldSentinel = "\x02"

// outsideCode applies fn to the stretches of text that are not code blocks
// (```...```) or inline code (`...`).
func outsideCode(text string, fn func(string) string) string {
	var out strings.Builder
	pos := 0
	for _, block := range codeBlockRe.FindAllStringIndex(text, -1) {
		out.WriteString(applyOutsideInline(text[pos:block[0]], fn))
		out.WriteString(text[block[0]:block[1]])
		pos = block[1]
	}
	out.WriteString(applyOutsideInline(text[pos:], fn))
	return out.String()
}

func applyOutsideInline(text string, fn func(string) string) string {
	var out strings.Builder
	pos := 0
	for _, span := range inlineCodeRe.FindAllStringIndex(text, -1) {
		out.WriteString(fn(text[pos:span[0]]))
		out.WriteString(text[span[0]:span[1]])
		pos = span[1]
	}
	out.WriteString(fn(text[pos:]))
	return out.String()
}

func convertMarker(text string, re *regexp.Regexp, open, close string) string {
	for {
		replaced := re.ReplaceAllString(text, "${1}"+open+"${2}"+close+"${3}")
		if replaced == text {
			return replaced
		}
		text = replaced
	}
}

// markdownToWhatsApp converts common Markdown to WhatsApp flavor
// (outbound).
func markdownToWhatsApp(text string) string {
	return outsideCode(text, func(part string) string {
		// Headers and bold go through a sentinel so the italic pass below
		// doesn't re-match the stars.
		part = mdHeaderRe.ReplaceAllString(part, boldSentinel+"$1"+boldSentinel)
		part = mdBoldRe.ReplaceAllString(part, boldSentinel+"$1"+boldSentinel)
		part = mdBoldUsRe.ReplaceAllString(part, boldSentinel+"$1"+boldSentinel)

		// Italic: *text* -> _text_ (markdown _text_ is already valid in WA)
		part = convertMarker(part, waBoldRe, "_", "_")

		// Strikethrough: ~~text~~ -> ~text~
		part = mdStrikeRe.ReplaceAllString(part, "~$1~")

		return strings.ReplaceAll(part, boldSentinel, "*")
	})
}

// whatsappToMarkdown converts WhatsApp flavor to common Markdown (inbound).
func whatsappToMarkdown(text string) string {
	return outsideCode(text, func(part string) string {
		part = convertMarker(part, waBoldRe, "**", "**")
		part = convertMarker(part, waStrikeRe, "~~", "~~")
		return part
	})
}
