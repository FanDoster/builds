// Builds — minimal client JS for polling and form interactions
document.addEventListener('DOMContentLoaded', () => {
  // Auto-refresh build detail page
  const logViewer = document.getElementById('build-log');
  if (logViewer && logViewer.dataset.status === 'running') {
    const buildId = logViewer.dataset.buildId;
    const poll = () => {
      fetch('/api/builds/' + buildId)
        .then(r => r.json())
        .then(build => {
          logViewer.textContent = build.log;
          if (build.status === 'running') {
            setTimeout(poll, 2000);
          } else {
            // Reload to show final state
            setTimeout(() => location.reload(), 1000);
          }
        })
        .catch(() => setTimeout(poll, 3000));
    };
    setTimeout(poll, 2000);
  }

  // Build trigger button
  const triggerBtn = document.getElementById('trigger-build');
  if (triggerBtn) {
    triggerBtn.addEventListener('click', () => {
      triggerBtn.disabled = true;
      triggerBtn.textContent = 'Triggering...';
      fetch(triggerBtn.dataset.url, { method: 'POST' })
        .then(r => r.json())
        .then(() => location.reload())
        .catch(() => {
          triggerBtn.disabled = false;
          triggerBtn.textContent = 'Build';
        });
    });
  }
});
