// Applies the saved theme before React renders, to avoid a flash of the wrong theme.
try {
  var t = localStorage.getItem('lakesync.theme')
  var dark = t === 'dark' || (t !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  if (dark) document.documentElement.classList.add('dark')
} catch (e) {}
