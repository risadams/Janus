/**
 * Colours the command at the start of each shell line. Prism's bash grammar
 * knows only builtins, so `janus doctor` would otherwise be one plain
 * colour, and most of the code in these docs is commands like it. A
 * grammar rule cannot see where a line starts once Prism has split the
 * text around other tokens, so this walks the tokens Prism produced
 * instead: a command is the first word of a line, or the first after a
 * pipe, `&&`, `||`, `;`, or `$(`, skipping any `NAME=value` set before it.
 */

const SHELLS = new Set(['bash', 'sh', 'shell', 'zsh']);
const SEPARATORS = new Set(['|', '||', '&&', ';', '&', '|&', '(', '$(', '`']);
const WORD = /^[A-Za-z.\/~][\w.\/~+-]*/;

function walk(Prism, tokens, state) {
  const out = [];
  for (const token of tokens) {
    if (typeof token !== 'string') {
      if (token.type === 'assign-left') {
        state.at = 'assign';
      } else if (SEPARATORS.has(String(token.content)) && ['operator', 'punctuation', 'variable'].includes(token.type)) {
        state.at = 'start';
      } else if (state.at === 'start' && token.type !== 'comment') {
        state.at = 'args';
      }
      if (Array.isArray(token.content)) {
        token.content = walk(Prism, token.content, state);
      }
      out.push(token);
      continue;
    }
    let text = token;
    let plain = '';
    while (text) {
      const c = text[0];
      if (c === '\n') {
        state.at = 'start';
      } else if (state.at === 'assign' && (c === ' ' || c === '\t')) {
        state.at = 'start';
      } else if (state.at === 'start' && c !== ' ' && c !== '\t') {
        const m = WORD.exec(text);
        state.at = 'args';
        if (m) {
          if (plain) out.push(plain);
          plain = '';
          out.push(new Prism.Token('function', m[0], 'command', m[0]));
          text = text.slice(m[0].length);
          continue;
        }
      }
      plain += c;
      text = text.slice(1);
    }
    if (plain) out.push(plain);
  }
  return out;
}

/** register adds the hook to a Prism instance. */
module.exports = function register(Prism) {
  Prism.hooks.add('after-tokenize', (env) => {
    if (SHELLS.has(env.language)) {
      env.tokens = walk(Prism, env.tokens, {at: 'start'});
    }
  });
};
