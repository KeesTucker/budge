// ---- Performance chart ----

function initBudgetPerfChart() {
    // A chart whose canvas has been swapped out is still registered, and
    // still listening for resizes, until it is destroyed.
    Object.values(Chart.instances).forEach(chart => {
        if (!chart.canvas.isConnected) chart.destroy();
    });
    const canvas = document.getElementById('budget-perf-chart');
    if (!canvas) return;
    const existing = Chart.getChart(canvas);
    if (existing) existing.destroy();
    const actuals = JSON.parse(canvas.dataset.actuals);
    const target  = parseFloat(canvas.dataset.target);
    const labels  = JSON.parse(canvas.dataset.labels);
    const goal    = parseFloat(canvas.dataset.savings) || 0;
    const moneyIn = JSON.parse(canvas.dataset.moneyIn || '[]');
    const income  = actuals.map((_, i) => moneyIn[i] || 0);
    const hasIncome = income.some(v => v > 0);

    // Each period split against the budget and against what actually came
    // in: pay, people paying you back, money from accounts Budge no longer
    // tracks. Spending up to the budget target is slate, past the target but
    // still within what came in is clay, and past what came in is brick on
    // top. What was left over is sage up to the savings goal, grey beyond.
    // The last bar is the week or month in progress: what it has not spent
    // yet is not saved yet either, and its pay may not have landed, so it is
    // only measured against the budget.
    const current = actuals.length - 1;
    const cap = actuals.map((_, i) => hasIncome && i !== current ? income[i] : Infinity);
    const budget = target > 0 ? target : Infinity;
    const within     = actuals.map((v, i) => Math.min(v, budget, cap[i]));
    const overBudget = actuals.map((v, i) => Math.max(Math.min(v, cap[i]) - within[i], 0));
    const overspent  = actuals.map((v, i) => Math.max(v - cap[i], 0));
    const saved       = actuals.map((v, i) => i === current ? 0 : Math.min(Math.max(income[i] - v, 0), goal));
    const unallocated = actuals.map((v, i) => i === current ? 0 : Math.max(income[i] - v - saved[i], 0));
    if (current >= 0) labels[current] += ' (so far)';

    const money = v => '$' + v.toLocaleString(undefined, {minimumFractionDigits: 2, maximumFractionDigits: 2});
    const bar = (label, data, colour) => ({
        type: 'bar', label, data, stack: 'period',
        backgroundColor: colour, borderWidth: 0, order: 2,
    });
    const datasets = [
        bar(target > 0 ? 'Within budget' : 'Spending', within, budge.colour('slate')),
        bar('Over budget', overBudget, budge.colour('clay')),
    ];
    if (hasIncome) {
        datasets.push(
            bar('Overspent', overspent, budge.colour('brick')),
            bar('Savings', saved, budge.colour('sage')),
            bar('Unallocated', unallocated, budge.mix('muted', 0.35)),
        );
    }
    // No lines for money in or the budget target: where slate turns clay is
    // the target, and where brick starts is what came in, so lines only
    // repeated the bars.

    new Chart(canvas, {
        data: {labels, datasets},
        options: {
            responsive: true,
            maintainAspectRatio: false,
            interaction: { mode: 'index' },
            scales: {
                x: { stacked: true },
                y: {
                    stacked: true,
                    beginAtZero: true,
                    ticks: { callback: v => '$' + v.toLocaleString() }
                }
            },
            plugins: {
                legend: { position: 'top' },
                tooltip: {
                    // Empty segments are left out, so a good week does not
                    // list "Overspent: $0.00".
                    filter: item => item.parsed.y > 0,
                    callbacks: {
                        label: ctx => ' ' + ctx.dataset.label + ': ' + money(ctx.parsed.y),
                        footer: items => 'Total spent: ' + money(actuals[items[0].dataIndex]),
                    }
                }
            }
        }
    });
}

// After settle, not swap. The canvas has an id, so htmx settles it: the new
// canvas briefly carries the old one's attributes, and gets its own back
// once settled. A chart drawn on swap was sized before that and then had
// its width and height taken from under it, leaving it squashed into the
// corner after a range change.
document.body.addEventListener('htmx:afterSettle', initBudgetPerfChart);
initBudgetPerfChart();

// ---- Save confirmation ----

function flashSaved(scope) {
    const badge = scope.querySelector('[data-saved-flash]');
    if (!badge) return;
    badge.classList.add('budge-flash-show');
    clearTimeout(badge._flashTimer);
    badge._flashTimer = setTimeout(() => badge.classList.remove('budge-flash-show'), 1500);
}

// ---- Target frequency conversion ----

function _toWeekly(v, f) {
    if (f === 'fortnightly') return v / 2;
    if (f === 'monthly')     return v * 12 / 52;
    if (f === 'yearly')      return v / 52;
    return v;
}
function _fromWeekly(v, f) {
    if (f === 'fortnightly') return v * 2;
    if (f === 'monthly')     return v * 52 / 12;
    if (f === 'yearly')      return v * 52;
    return v;
}
function convertTargetFrequency(sel) {
    const prev = sel.dataset.prev || 'weekly';
    const next = sel.value;
    if (prev === next) return;
    const amountEl = sel.closest('form').querySelector('input[name="target_amount"]');
    if (amountEl && amountEl.value !== '') {
        const weekly = _toWeekly(parseFloat(amountEl.value), prev);
        amountEl.value = _fromWeekly(weekly, next).toFixed(2);
    }
    sel.dataset.prev = next;
}
