document.addEventListener('htmx:configRequest', function(e) {
    var token = document.querySelector('meta[name="csrf-token"]');
    if (token) {
        e.detail.headers['X-CSRF-Token'] = token.getAttribute('content');
    }
});
