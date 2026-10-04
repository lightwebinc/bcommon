/**
 * The renderer filter: the twin of the Go package sanitize. It applies four
 * rules, in order, each to what the one before it returned, by one pinned
 * Unicode property table, so that a web renderer shows the same characters
 * as a terminal one:
 *
 *  1. a tab becomes a space, and U+2028 and U+2029 become LF;
 *  2. every control character but LF (C0, DEL and C1) goes, every escape
 *     sequence goes whole, the bidirectional controls, the zero-width and
 *     invisible characters and the supplementary variation selectors go, and
 *     every tag character goes unless its run, with the character kept before
 *     it, is an emoji tag sequence the table lists, which stays whole;
 *  3. U+FE00 to U+FE0D go, and U+FE0E or U+FE0F stays only where, with the
 *     character kept before it, it forms an emoji variation sequence the
 *     table lists;
 *  4. U+200D stays only where the nearest character kept before it, passing
 *     over U+FE0F and emoji modifiers, and the character after it are both
 *     Extended_Pictographic.
 *
 * The table is sanitize-table.ts, generated with the Go package's from
 * Unicode 15.1's emoji data and holding the same bytes; neither language
 * uses its runtime's own Unicode properties. The output is text: a web page
 * inserts it as a DOM text node, never as markup.
 */
import { unicodeTableJSON } from './sanitize-table.js'

/** The version of the Unicode data the table holds. */
export const UnicodeVersion = '15.1'

interface Table {
  unicode: string
  extendedPictographic: Array<[number, number]>
  emojiModifier: Array<[number, number]>
  textVariationBases: number[]
  emojiVariationBases: number[]
  tagSequences: number[][]
}

const table = JSON.parse(unicodeTableJSON) as Table
if (table.unicode !== UnicodeVersion) {
  throw new Error(`sanitize: the table is Unicode ${table.unicode}`)
}
const textBases = new Set(table.textVariationBases)
const emojiBases = new Set(table.emojiVariationBases)
const tagSequences = new Set(table.tagSequences.map((s) => s.join(' ')))

function inRanges(rs: Array<[number, number]>, cp: number): boolean {
  let lo = 0
  let hi = rs.length
  while (lo < hi) {
    const mid = (lo + hi) >>> 1
    if ((rs[mid] as [number, number])[1] < cp) lo = mid + 1
    else hi = mid
  }
  const r = rs[lo]
  return r !== undefined && r[0] <= cp
}

const pictographic = (cp: number): boolean => inRanges(table.extendedPictographic, cp)
const modifier = (cp: number): boolean => inRanges(table.emojiModifier, cp)

const control = (cp: number): boolean => cp < 0x20 || cp === 0x7f || (cp >= 0x80 && cp <= 0x9f)
const tagChar = (cp: number): boolean => cp >= 0xe0000 && cp <= 0xe007f

function hidden(cp: number): boolean {
  return (
    // bidirectional controls
    cp === 0x061c ||
    cp === 0x200e ||
    cp === 0x200f ||
    (cp >= 0x202a && cp <= 0x202e) ||
    (cp >= 0x2066 && cp <= 0x2069) ||
    // zero-width and invisible
    cp === 0x00ad ||
    cp === 0x034f ||
    cp === 0x115f ||
    cp === 0x1160 ||
    cp === 0x180e ||
    cp === 0x200b ||
    cp === 0x200c ||
    (cp >= 0x2060 && cp <= 0x2064) ||
    cp === 0x3164 ||
    cp === 0xfeff ||
    cp === 0xffa0 ||
    // supplementary variation selectors
    (cp >= 0xe0100 && cp <= 0xe01ef)
  )
}

/** The code points of s, each lone surrogate as U+FFFD. */
function codePoints(s: string): number[] {
  const out: number[] = []
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      const d = s.charCodeAt(i + 1)
      if (d >= 0xdc00 && d <= 0xdfff) {
        out.push(0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00))
        i++
        continue
      }
    }
    out.push(c >= 0xd800 && c <= 0xdfff ? 0xfffd : c)
  }
  return out
}

/** Rule 1. */
function mapSpaces(cps: number[]): number[] {
  return cps.map((cp) => (cp === 0x09 ? 0x20 : cp === 0x2028 || cp === 0x2029 ? 0x0a : cp))
}

/** Rule 2. Its escape-sequence states are the Go termsafe package's. */
function removeHidden(cps: number[]): number[] {
  const Text = 0
  const AfterEsc = 1
  const Csi = 2
  const Str = 3
  const StrEsc = 4
  const TwoChar = 5
  const out: number[] = []
  let state = Text
  for (let i = 0; i < cps.length; i++) {
    const cp = cps[i] as number
    switch (state) {
      case AfterEsc:
        if (cp === 0x5b) state = Csi
        else if (cp === 0x5d || cp === 0x50 || cp === 0x58 || cp === 0x5e || cp === 0x5f) state = Str
        else if (cp >= 0x20 && cp <= 0x2f) state = TwoChar
        else state = Text
        continue
      case Csi:
        if ((cp >= 0x40 && cp <= 0x7e) || cp < 0x20) state = Text
        continue
      case Str:
        if (cp === 0x07 || cp === 0x9c) state = Text
        else if (cp === 0x1b) state = StrEsc
        continue
      case StrEsc:
        state = cp === 0x5c ? Text : Str
        continue
      case TwoChar:
        if ((cp >= 0x30 && cp <= 0x7e) || cp < 0x20) state = Text
        continue
    }
    if (cp === 0x0a) {
      out.push(cp)
    } else if (cp === 0x1b) {
      state = AfterEsc
    } else if (cp === 0x9b) {
      state = Csi
    } else if (control(cp) || hidden(cp)) {
      // removed
    } else if (tagChar(cp)) {
      let end = i + 1
      while (end < cps.length && tagChar(cps[end] as number)) end++
      const run = cps.slice(i, end)
      if (out.length > 0 && tagSequences.has([out[out.length - 1] as number, ...run].join(' '))) {
        out.push(...run)
      }
      i = end - 1
    } else {
      out.push(cp)
    }
  }
  return out
}

/** Rule 3. */
function keepSelectors(cps: number[]): number[] {
  const out: number[] = []
  for (const cp of cps) {
    if (cp >= 0xfe00 && cp <= 0xfe0d) continue
    if (cp === 0xfe0e && (out.length === 0 || !textBases.has(out[out.length - 1] as number))) continue
    if (cp === 0xfe0f && (out.length === 0 || !emojiBases.has(out[out.length - 1] as number))) continue
    out.push(cp)
  }
  return out
}

/** Rule 4. */
function keepJoiners(cps: number[]): number[] {
  const out: number[] = []
  for (let i = 0; i < cps.length; i++) {
    const cp = cps[i] as number
    if (cp === 0x200d) {
      let k = out.length - 1
      while (k >= 0 && (out[k] === 0xfe0f || modifier(out[k] as number))) k--
      const next = cps[i + 1]
      if (k < 0 || !pictographic(out[k] as number) || next === undefined || !pictographic(next)) continue
    }
    out.push(cp)
  }
  return out
}

function filterCodePoints(cps: number[]): string {
  const out = keepJoiners(keepSelectors(removeHidden(mapSpaces(cps))))
  let s = ''
  for (let i = 0; i < out.length; i += 4096) {
    s += String.fromCodePoint(...out.slice(i, i + 4096))
  }
  return s
}

/**
 * filterText applies the four rules to s, a lone surrogate first becoming
 * U+FFFD. It never returns a control character but LF, nor any character
 * rule 2 lists.
 */
export function filterText(s: string): string {
  return filterCodePoints(codePoints(s))
}

/**
 * filterBytes is filterText over UTF-8 bytes read from elsewhere (a fetched
 * attachment's name, say): invalid UTF-8 first becomes U+FFFD as the WHATWG
 * decoder replaces it, one for each maximal ill-formed subsequence, which is
 * what the Go Filter does with the same bytes. A leading byte order mark is
 * kept for rule 2 to remove, not consumed by the decoder.
 */
export function filterBytes(b: Uint8Array): string {
  return filterText(new TextDecoder('utf-8', { fatal: false, ignoreBOM: true }).decode(b))
}
