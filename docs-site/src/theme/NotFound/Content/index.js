import React from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import { PageMetadata, useColorMode } from '@docusaurus/theme-common';

// The page for a URL that leads nowhere, in place of Docusaurus's, which
// asks the reader to tell the site's owner. It uses the homepage's hero
// and documentation cards, and offers the places people were most likely
// looking for. It sets its own title, since a missing page under /docs
// is drawn by the docs layout, which gives it none.
const places = [
  { title: 'User guide', desc: 'Install, configure, and use Janus, from the start.', to: '/docs/guide', accent: 'rose' },
  { title: 'Configuration', desc: 'Every setting in janus.config.json.', to: '/docs/CONFIG', accent: 'navy' },
  { title: 'Troubleshooting', desc: 'First steps, then problems by symptom.', to: '/docs/TROUBLESHOOTING', accent: 'amber' },
];

export default function NotFoundContent() {
  const { colorMode } = useColorMode();
  const light = useBaseUrl('/img/logo.svg');
  const dark = useBaseUrl('/img/logo-dark.svg');
  return (
    <main>
      <PageMetadata title="Page not found" />
      <section className="hero-banner">
        <img src={colorMode === 'dark' ? dark : light} alt="" width="88" height="82" style={{ marginBottom: '1.25rem' }} />
        <div className="hero-eyebrow">page not found</div>
        <h1 className="hero-title">
          Janus looked both ways<br />
          <em className="hero-title-em">and found nothing.</em>
        </h1>
        <p className="hero-subtitle">
          There is no page at this address. It may have moved when the docs
          were reorganised; one of these is probably what you were after.
        </p>
      </section>
      <section className="doc-links-section">
        <div className="doc-links-grid doc-links-grid--three">
          {places.map(({ title, desc, to, accent }) => (
            <Link key={title} className={`doc-card doc-card--${accent}`} to={to}>
              <div className="doc-card-title">{title}</div>
              <div className="doc-card-desc">{desc}</div>
            </Link>
          ))}
        </div>
        <p className="not-found-report">
          Followed a link here from these docs?{' '}
          <Link to="https://github.com/risadams/Janus/issues/new?labels=documentation&title=Broken+link+on+the+docs+site">
            Say which one
          </Link>
          , and it will be fixed.
        </p>
      </section>
    </main>
  );
}
