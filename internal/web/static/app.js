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
        var rows = document.querySelectorAll('#repos-table-body tr');
        rows.forEach(function(row) {
            var text = row.textContent.toLowerCase();
            row.style.display = text.indexOf(q) === -1 ? 'none' : '';
        });
    });
});

// Confirmation via data-confirm attribute
document.addEventListener('htmx:confirm', function(e) {
    var trigger = e.detail.elt;
    var confirmEl = trigger.closest('[data-confirm]');
    if (!confirmEl) return;
    if (!confirm(confirmEl.dataset.confirm)) {
        e.preventDefault();
    }
});

// Confirmation for native (non-htmx) form submissions with data-confirm
document.addEventListener('submit', function(e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    var msg = form.getAttribute('data-confirm');
    if (msg && !confirm(msg)) {
        e.preventDefault();
    }
});

// Select-all checkbox
document.addEventListener('DOMContentLoaded', function() {
    function syncSelectAll(checked) {
        document.querySelectorAll('.select-all').forEach(function(cb) {
            cb.checked = checked;
        });
    }

    document.querySelectorAll('.select-all').forEach(function(selectAll) {
        selectAll.addEventListener('change', function() {
            syncSelectAll(selectAll.checked);
            document.querySelectorAll('.repo-check').forEach(function(cb) {
                cb.checked = selectAll.checked;
            });
            updateActionButtons();
        });
    });

    document.addEventListener('change', function(e) {
        if (e.target.classList.contains('repo-check')) {
            updateActionButtons();
        }
    });
});

function updateActionButtons() {
    var checked = document.querySelectorAll('.repo-check:checked').length;
    ['btn-set-clone', 'btn-set-bundle', 'btn-enable-backup', 'btn-disable-backup'].forEach(function(id) {
        var btn = document.getElementById(id);
        if (btn) btn.disabled = checked === 0;
    });
}

// Wire bulk action buttons
document.addEventListener('DOMContentLoaded', function() {
    function flashButton(btn, ok, original) {
        btn.textContent = ok ? '✓ Done' : '✗ Failed';
        btn.disabled = true;
        if (btn._flashTimer) {
            clearTimeout(btn._flashTimer);
        }
        btn._flashTimer = setTimeout(function() {
            btn.textContent = original;
            btn.disabled = false;
            btn._flashTimer = null;
            updateActionButtons();
        }, 1500);
    }

    function setupBulkButton(btnId, url, extraParams) {
        var btn = document.getElementById(btnId);
        if (!btn) return;
        btn.addEventListener('click', function() {
            var ids = [];
            document.querySelectorAll('.repo-check:checked').forEach(function(cb) {
                ids.push(cb.value);
            });
            if (ids.length === 0) return;
            var original = btn.textContent;
            btn.textContent = 'Working…';
            btn.disabled = true;
            var params = Object.assign({ids: ids.join(',')}, extraParams);
            fetch(url, {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/x-www-form-urlencoded',
                    'X-CSRF-Token': (document.querySelector('meta[name="csrf-token"]') || {}).content || ''
                },
                body: new URLSearchParams(params)
            }).then(function(res) {
                flashButton(btn, res.ok, original);
            }).catch(function() {
                flashButton(btn, false, original);
            });
        });
    }
    setupBulkButton('btn-set-clone', '/bulk/set-format', {format: 'clone'});
    setupBulkButton('btn-set-bundle', '/bulk/set-format', {format: 'bundle'});
    setupBulkButton('btn-enable-backup', '/bulk/set-backup', {backup_enabled: 'on'});
    setupBulkButton('btn-disable-backup', '/bulk/set-backup', {backup_enabled: 'off'});
});
