/**
 * The Ris Ink code palette ("parchment-dark", from the brand guide in
 * risadams/vs-ris BRAND.md): code sits on dark parchment in both colour
 * modes, a little lighter on a light page. Every colour passes 4.5:1 on
 * both backgrounds.
 */
const colours = {
  text: '#f0e6d2',
  comment: '#a49a8e',
  keyword: '#e8659a',
  fn: '#b183b5',
  string: '#5ba3b8',
  number: '#c48a1e',
  type: '#b8875f',
  punctuation: '#c8b89a',
};

function theme(backgroundColor) {
  return {
    plain: {color: colours.text, backgroundColor},
    styles: [
      {types: ['comment', 'prolog', 'doctype', 'cdata', 'shebang'], style: {color: colours.comment, fontStyle: 'italic'}},
      {types: ['keyword', 'property', 'tag', 'selector', 'important', 'atrule', 'builtin', 'deleted'], style: {color: colours.keyword}},
      {types: ['function', 'operator', 'entity', 'url'], style: {color: colours.fn}},
      {types: ['string', 'char', 'attr-value', 'attr-name', 'regex', 'inserted'], style: {color: colours.string}},
      {types: ['number', 'boolean', 'constant', 'symbol', 'unit'], style: {color: colours.number}},
      {types: ['class-name', 'namespace', 'variable', 'parameter'], style: {color: colours.type}},
      {types: ['punctuation'], style: {color: colours.punctuation}},
    ],
  };
}

module.exports = {
  light: theme('#2a2220'),
  dark: theme('#1a1210'),
};
