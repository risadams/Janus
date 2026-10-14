import React from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import { useColorMode } from '@docusaurus/theme-common';

const features = [
  {
    title: 'See what every request costs',
    body: 'A ledger line for each request, and janus stats to add them up by kind of request, model, and backend, with prompt-cache hits and cost at list price.',
  },
  {
    title: 'Untouched until you say so',
    body: 'With every feature off, what reaches Anthropic and what comes back match a direct connection. Your login passes through, and is never stored or logged.',
  },
  {
    title: 'Local first, Claude as the net',
    body: 'Send subagent turns to a model in LM Studio. Janus checks each answer before you see it, and hands the turn to Claude when it fails.',
  },
  {
    title: 'Keep working when Claude is down',
    body: 'At a usage limit or an outage, the local model stands in. Janus loads it if needed, queues requests, and fits a long conversation to its context.',
  },
  {
    title: 'Secrets stay on your machine',
    body: 'Credentials it recognises become placeholders before a request leaves, and are put back in the answer. Files you name never reach Anthropic at all.',
  },
  {
    title: 'Kind to the prompt cache',
    body: 'Anything already sent goes out in the same bytes on every later turn, so Claude Code keeps reading from its cache and nothing costs more than it should.',
  },
];

const docLinks = [
  {
    title: 'Install',
    desc: 'macOS, Linux, Windows, LM Studio, and running it in the background.',
    to: '/docs/guide/install',
    accent: 'rose',
  },
  {
    title: 'Configure',
    desc: 'The config file, and complete setups to start from.',
    to: '/docs/guide/configure',
    accent: 'navy',
  },
  {
    title: 'Use',
    desc: 'The console, stats, audit, and what an outage looks like.',
    to: '/docs/guide/use',
    accent: 'plum',
  },
  {
    title: 'Troubleshoot',
    desc: 'First steps, then problems by symptom.',
    to: '/docs/TROUBLESHOOTING',
    accent: 'amber',
  },
];

function Hero() {
  const { colorMode } = useColorMode();
  const light = useBaseUrl('/img/logo.svg');
  const dark = useBaseUrl('/img/logo-dark.svg');
  const mark = colorMode === 'dark' ? dark : light;
  return (
    <section className="hero-banner">
      <img src={mark} alt="" width="88" height="82" style={{ marginBottom: '1.25rem' }} />
      <div className="hero-eyebrow">a local proxy for claude code</div>
      <h1 className="hero-title">
        Janus<br />
        <em className="hero-title-em">looks both ways.</em>
      </h1>
      <p className="hero-subtitle">
        It sits on your machine between Claude Code and the Anthropic API,
        keeps the books on every request, and, when you switch them on, sends
        chosen work to a local model, keeps secrets in, and keeps you working
        when Claude is down.
      </p>
      <div className="hero-buttons">
        <Link className="hero-button-primary" to="/docs/guide">
          Get started
        </Link>
        <Link className="hero-button-secondary" to="/docs/CONFIG">
          Configuration reference
        </Link>
      </div>
    </section>
  );
}

function QuickInstall() {
  return (
    <section className="quick-install-section">
      <div className="quick-install-block">
        <div className="quick-install-label">Quick start</div>
        <pre className="quick-install-pre">{
`# Install with Go 1.27+, or download a release build
go install github.com/risadams/janus/cmd/janus@latest

# Check the setup, then start it
janus doctor
janus

# In another terminal, point Claude Code at it
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ENABLE_TOOL_SEARCH=true claude`
        }</pre>
      </div>
    </section>
  );
}

function FeaturesSection() {
  return (
    <section className="features-section">
      <div className="features-header">
        <div className="features-eyebrow">what it does</div>
        <h2 className="features-heading">
          Every request, <em className="features-heading-em">accounted for.</em>
        </h2>
        <p className="features-subtitle">
          Out of the box Janus only keeps the books. Everything else is a
          switch you turn on when you want it.
        </p>
      </div>
      <div className="feature-grid">
        {features.map(({ title, body }, i) => (
          <div key={title} className="feature-card">
            <div
              className="feature-card-icon"
              aria-hidden="true"
              style={{ fontFamily: "'JetBrains Mono', monospace", fontSize: '1rem', color: 'var(--dr-rose)' }}
            >
              {String(i + 1).padStart(2, '0')}
            </div>
            <p className="feature-card-title">{title}</p>
            <p className="feature-card-body">{body}</p>
          </div>
        ))}
      </div>
    </section>
  );
}

function DocLinksSection() {
  return (
    <section className="doc-links-section">
      <h2 className="doc-links-heading">Documentation</h2>
      <div className="doc-links-grid">
        {docLinks.map(({ title, desc, to, accent }) => (
          <Link key={title} className={`doc-card doc-card--${accent}`} to={to}>
            <div className="doc-card-title">{title}</div>
            <div className="doc-card-desc">{desc}</div>
          </Link>
        ))}
      </div>
    </section>
  );
}

export default function Home() {
  const { siteConfig } = useDocusaurusContext();
  return (
    <Layout title="A local proxy for Claude Code" description={siteConfig.tagline}>
      <main>
        <Hero />
        <QuickInstall />
        <FeaturesSection />
        <DocLinksSection />
      </main>
    </Layout>
  );
}
