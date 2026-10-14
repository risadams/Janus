import React from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import { useColorMode } from '@docusaurus/theme-common';

// The Janus mark and name. The dark-mode mark uses the lighter teal and
// rose, since the full colours are hard to see on the dark navbar.
export default function NavbarLogo() {
  const { colorMode } = useColorMode();
  const light = useBaseUrl('/img/logo.svg');
  const dark = useBaseUrl('/img/logo-dark.svg');
  return (
    <Link className="navbar__brand" to="/">
      <img
        className="navbar__logo"
        src={colorMode === 'dark' ? dark : light}
        alt=""
        width="32"
        height="30"
      />
      <b className="navbar__title">Janus</b>
    </Link>
  );
}
