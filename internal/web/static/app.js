// htmx CSRF header injection
document.addEventListener('htmx:configRequest', function(e) {
    var token = document.querySelector('meta[name="csrf-token"]');
    if (token && token.content) {
        e.detail.headers['X-CSRF-Token'] = token.content;
    }
});

// Auto-dismiss flash messages after 5 seconds
document.addEventListener('DOMContentLoaded', function() {
    var alerts = document.querySelectorAll('.alert');
    alerts.forEach(function(alert) {
        setTimeout(function() { alert.remove(); }, 5000);
    });
});

// Client-side repo filter
document.addEventListener('DOMContentLoaded', function() {
    var search = document.querySelector('input[type="search"][name="q"]');
    if (!search) return;
    search.addEventListener('input', function() {
        var q = this.value.toLowerCase();
        var rows = document.querySelectorAll('#repos-table tbody tr');
        rows.forEach(function(row) {
            var text = row.textContent.toLowerCase();
            row.style.display = text.indexOf(q) === -1 ? 'none' : '';
        });
    });
});
