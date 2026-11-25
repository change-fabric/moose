// Runs axe-core over every .html file in a directory, in headless Chromium, at a
// desktop width and at 320px (WCAG reflow, the same layout as 400% zoom on a
// desktop). It runs inside ghcr.io/browserless/chromium, which ships
// playwright-core, a matching Chromium and axe-core:
//
//   docker run --rm --entrypoint node -v "$PWD/dist/axe:/pages:ro" \
//     -v "$PWD/scripts/axe-pages.mjs:/axe-pages.mjs:ro" ghcr.io/browserless/chromium:v2.38.1 \
//     /axe-pages.mjs /pages
//
// It exits 1 when any page has a violation of WCAG 2.0/2.1/2.2 A or AA or an
// axe best practice.
import { createRequire } from 'node:module';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const require = createRequire('/usr/src/app/');
const { chromium } = require('playwright-core');
const axeSource = readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8');

const dir = process.argv[2];
if (!dir) {
  console.error('usage: axe-pages.mjs <dir of .html files>');
  process.exit(2);
}
const files = readdirSync(dir).filter((f) => f.endsWith('.html')).sort();
if (files.length === 0) {
  console.error(`no .html files in ${dir}`);
  process.exit(2);
}

const viewports = [
  { name: '1280x800', width: 1280, height: 800 },
  { name: '320x640', width: 320, height: 640 },
];
const tags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'];

const browser = await chromium.launch({ args: ['--no-sandbox'] });
let failed = 0;
for (const file of files) {
  for (const vp of viewports) {
    const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, bypassCSP: true });
    const page = await context.newPage();
    await page.goto('file://' + join(dir, file));
    await page.addScriptTag({ content: axeSource });
    const result = await page.evaluate((runOnlyTags) => axe.run(document, { runOnly: { type: 'tag', values: runOnlyTags } }), tags);
    // No horizontal page scroll at the narrow width.
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    const problems = result.violations.length + (overflow > 0 ? 1 : 0);
    console.log(`${problems === 0 ? 'ok  ' : 'FAIL'} ${file} @ ${vp.name} (${result.passes.length} rules passed)`);
    for (const v of result.violations) {
      failed++;
      console.log(`  - ${v.id} [${v.impact}] ${v.help}`);
      for (const n of v.nodes.slice(0, 3)) console.log(`      ${n.target.join(' ')}: ${n.failureSummary.split('\n').slice(1).join(' ')}`);
    }
    if (overflow > 0) {
      failed++;
      console.log(`  - horizontal scroll: the page is ${overflow}px wider than the viewport`);
    }
    await context.close();
  }
}
await browser.close();
console.log(failed === 0 ? `axe: no violations in ${files.length} pages` : `axe: ${failed} problems`);
process.exit(failed === 0 ? 0 : 1);
