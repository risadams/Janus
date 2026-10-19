/**
 * Loads the languages listed in prism.additionalLanguages, as Docusaurus's
 * own version of this file does, then adds the hook that colours shell
 * commands (src/prism/shell-commands.js).
 */
import siteConfig from '@generated/docusaurus.config';
import registerShellCommands from '../prism/shell-commands';

export default function prismIncludeLanguages(PrismObject) {
  const {
    themeConfig: {prism},
  } = siteConfig;
  // Prism components work on the Prism instance on the window, while
  // prism-react-renderer uses its own; mount it there while they load.
  const PrismBefore = globalThis.Prism;
  globalThis.Prism = PrismObject;
  prism.additionalLanguages.forEach((lang) => {
    // eslint-disable-next-line global-require, import/no-dynamic-require
    require(`prismjs/components/prism-${lang}`);
  });
  delete globalThis.Prism;
  if (typeof PrismBefore !== 'undefined') {
    globalThis.Prism = PrismBefore;
  }

  registerShellCommands(PrismObject);
}
