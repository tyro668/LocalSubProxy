import { Parser, fromFile } from '@asyncapi/parser';
const p = new Parser();
const { document, diagnostics } = await fromFile(p, process.argv[2]).parse();
const errs = diagnostics.filter(d => d.severity === 0);
for (const d of diagnostics) console.log(['error','warn','info','hint'][d.severity], d.code, d.path.join('.'), d.message);
console.log(document && errs.length===0 ? 'ASYNCAPI VALID' : 'ASYNCAPI INVALID');
process.exit(errs.length?1:0);
