// Validate mermaid blocks in markdown using mermaid's own parser.
import fs from 'node:fs';
import { JSDOM } from 'jsdom';
const dom = new JSDOM('<!DOCTYPE html><body></body>', { pretendToBeVisual: true });
global.window = dom.window; global.document = dom.window.document;
global.HTMLElement = dom.window.HTMLElement;
Object.defineProperty(global, "navigator", { value: dom.window.navigator, configurable: true });
global.DOMPurify = undefined;
const mermaid = (await import('mermaid')).default;
mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' });
let bad = 0, total = 0;
for (const file of process.argv.slice(2)) {
  const src = fs.readFileSync(file, 'utf8');
  const blocks = [...src.matchAll(/```mermaid\n([\s\S]*?)```/g)];
  for (const [i, m] of blocks.entries()) {
    total++;
    try { await mermaid.parse(m[1]); }
    catch (e) { bad++; console.log(`FAIL ${file} block#${i+1}: ${String(e.message).split('\n')[0]}`); }
  }
}
console.log(`mermaid blocks checked: ${total}, failures: ${bad}`);
process.exit(bad ? 1 : 0);
