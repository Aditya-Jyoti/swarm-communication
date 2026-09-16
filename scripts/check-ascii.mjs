// CLAUDE.md 4.1: generated markdown must be strict ASCII.
// Non-ASCII slips in invisibly (em dashes, smart quotes, box-drawing left over
// from an ASCII diagram), so this is enforced mechanically rather than by review.
import fs from 'node:fs';

const NAMES = {
  '—': 'em dash (use --)', '–': 'en dash (use -)',
  '‘': 'left single quote', '’': 'right single quote (use \')',
  '“': 'left double quote', '”': 'right double quote (use ")',
  '…': 'ellipsis (use ...)', '×': 'multiplication sign (use x or \\times)',
  '→': 'right arrow (use ->)', '≤': 'less-or-equal (use <=)',
  '≥': 'greater-or-equal (use >=)', '≈': 'almost-equal (use ~=)',
  '·': 'middle dot', '✓': 'check mark', ' ': 'non-breaking space'
};

let bad = 0;
for (const file of process.argv.slice(2)) {
  const lines = fs.readFileSync(file, 'utf8').split('\n');
  lines.forEach((line, i) => {
    for (const ch of new Set(line.match(/[^\x00-\x7F]/g) || [])) {
      const label = NAMES[ch] || `U+${ch.codePointAt(0).toString(16).toUpperCase().padStart(4, '0')}`;
      console.log(`${file}:${i + 1}: non-ASCII ${JSON.stringify(ch)} -- ${label}`);
      bad++;
    }
  });
}
console.log(bad ? `\nFAIL: ${bad} non-ASCII occurrence(s).` : 'OK: all files are strict ASCII.');
process.exit(bad ? 1 : 0);
