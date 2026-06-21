function initLightbox() {
  var overlay = document.getElementById('lightbox-overlay');
  if (!overlay) {
    overlay = document.createElement('div');
    overlay.id = 'lightbox-overlay';
    overlay.style.cssText =
      'position:fixed;inset:0;z-index:9999;background:rgba(0,0,0,0.92);display:none;cursor:zoom-out;justify-content:center;align-items:center;';
    overlay.innerHTML =
      '<img style="max-width:95vw;max-height:95vh;object-fit:contain;" />';
    overlay.addEventListener('click', function () {
      overlay.style.display = 'none';
    });
    document.body.appendChild(overlay);
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && overlay.style.display === 'flex') {
        overlay.style.display = 'none';
      }
    });
  }

  document
    .querySelectorAll('[data-lightbox], .hero-html')
    .forEach(function (el) {
      var img = el.querySelector('img');
      if (!img || el.dataset.lightboxBound) return;
      el.dataset.lightboxBound = '1';
      img.style.cursor = 'zoom-in';
      img.addEventListener('click', function (e) {
        e.stopPropagation();
        var overlayImg = overlay.querySelector('img');
        overlayImg.src = img.src;
        overlayImg.alt = img.alt;
        overlay.style.display = 'flex';
      });
    });
}

document.addEventListener('DOMContentLoaded', initLightbox);
window.addEventListener('pageshow', initLightbox);

if (window.document$ && typeof window.document$.subscribe === 'function') {
  window.document$.subscribe(initLightbox);
}
