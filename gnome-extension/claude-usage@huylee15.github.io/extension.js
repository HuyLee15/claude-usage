import Clutter from 'gi://Clutter';
import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import GObject from 'gi://GObject';
import St from 'gi://St';

import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import * as PanelMenu from 'resource:///org/gnome/shell/ui/panelMenu.js';
import * as PopupMenu from 'resource:///org/gnome/shell/ui/popupMenu.js';

// The CLI caches results and backs off on 429 itself, so polling it is cheap; this only bounds staleness.
const POLL_SECONDS = 120;
// A weekly window this full blocks the account regardless of the 5-hour number, so the panel calls it out.
const WEEK_ALERT = 90;

// GNOME Shell's PATH usually lacks ~/.local/bin, where `make install` puts the binary.
function findBinary() {
    return GLib.find_program_in_path('claude-usage') ??
        GLib.build_filenamev([GLib.get_home_dir(), '.local', 'bin', 'claude-usage']);
}

function level(utilization) {
    if (utilization >= 90)
        return 'crit';
    if (utilization >= 70)
        return 'warn';
    return 'ok';
}

function pct(win) {
    return win ? `${Math.round(win.utilization)}%` : '—';
}

function formatDuration(ms) {
    const minutes = Math.floor(ms / 60000);
    const days = Math.floor(minutes / 1440);
    const hours = Math.floor(minutes / 60) % 24;
    if (days > 0)
        return `${days}d${hours}h`;
    if (hours > 0)
        return `${hours}h${String(minutes % 60).padStart(2, '0')}m`;
    if (minutes > 0)
        return `${minutes}m`;
    return `${Math.max(0, Math.floor(ms / 1000))}s`;
}

function resets(win) {
    if (!win?.resets_at)
        return '';
    const ms = Date.parse(win.resets_at) - Date.now();
    return ms > 0 ? ` (${formatDuration(ms)})` : ' (now)';
}

function describe(result) {
    const lines = [`${result.name}  ${result.email ?? ''}`];
    const u = result.usage;
    if (u) {
        lines.push(`  5h ${pct(u.five_hour)}${resets(u.five_hour)}   week ${pct(u.seven_day)}${resets(u.seven_day)}` +
            `   sonnet ${pct(u.seven_day_sonnet)}   opus ${pct(u.seven_day_opus)}`);
    }
    if (result.error) {
        const age = result.stale && result.fetched_at
            ? `as of ${formatDuration(Date.now() - Date.parse(result.fetched_at))} ago: ` : '';
        lines.push(`  ${age}${result.error}`);
    }
    return lines.join('\n');
}

const UsageIndicator = GObject.registerClass(
class UsageIndicator extends PanelMenu.Button {
    _init() {
        super._init(0.0, 'Claude Usage');
        this._box = new St.BoxLayout({style_class: 'claude-usage-box', y_align: Clutter.ActorAlign.CENTER});
        this._box.add_child(new St.Label({text: 'Claude …', y_align: Clutter.ActorAlign.CENTER}));
        this.add_child(this._box);

        this._accounts = new PopupMenu.PopupMenuSection();
        this.menu.addMenuItem(this._accounts);
        this.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        this._status = new PopupMenu.PopupMenuItem('Loading…', {reactive: false});
        this.menu.addMenuItem(this._status);
        const refreshItem = new PopupMenu.PopupMenuItem('Refresh now');
        refreshItem.connect('activate', () => this.refresh(true));
        this.menu.addMenuItem(refreshItem);
        // Opening the menu re-reads the CLI cache so countdowns are current; it does not hit the API early.
        this.menu.connect('open-state-changed', (_menu, open) => {
            if (open)
                this.refresh(false);
        });

        this._proc = null;
        this._cancellable = null;
    }

    refresh(force) {
        if (this._proc)
            return;
        const argv = [findBinary(), '--json'];
        if (force)
            argv.push('--force');
        let proc;
        try {
            proc = Gio.Subprocess.new(argv, Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE);
        } catch (e) {
            this._showFailure(`cannot run ${argv[0]}: ${e.message}`);
            return;
        }
        this._proc = proc;
        this._cancellable = new Gio.Cancellable();
        proc.communicate_utf8_async(null, this._cancellable, (p, res) => {
            this._proc = null;
            this._cancellable = null;
            let stdout, stderr;
            try {
                [, stdout, stderr] = p.communicate_utf8_finish(res);
            } catch (e) {
                if (!e.matches?.(Gio.IOErrorEnum, Gio.IOErrorEnum.CANCELLED))
                    this._showFailure(e.message);
                return;
            }
            if (!p.get_successful()) {
                this._showFailure((stderr || stdout || 'claude-usage failed').trim().replace(/^error: /, ''));
                return;
            }
            try {
                this._render(JSON.parse(stdout));
            } catch (e) {
                this._showFailure(`unexpected output: ${e.message}`);
            }
        });
    }

    _render(results) {
        this._box.destroy_all_children();
        this._accounts.removeAll();
        for (const r of results) {
            const five = r.usage?.five_hour;
            const week = r.usage?.seven_day;
            const worst = Math.max(five?.utilization ?? 0, week?.utilization ?? 0);
            let text = five ? pct(five) : '—';
            if ((week?.utilization ?? 0) >= WEEK_ALERT)
                text += `·w${pct(week)}`;
            let cls = r.usage ? `claude-usage-${level(worst)}` : 'claude-usage-none';
            if (r.stale)
                cls = 'claude-usage-stale';
            this._box.add_child(new St.Label({text: r.name, style_class: 'claude-usage-name', y_align: Clutter.ActorAlign.CENTER}));
            this._box.add_child(new St.Label({text, style_class: cls, y_align: Clutter.ActorAlign.CENTER}));

            const item = new PopupMenu.PopupMenuItem(describe(r), {reactive: false});
            item.label.add_style_class_name(r.error && !r.usage ? 'claude-usage-error' : 'claude-usage-detail');
            this._accounts.addMenuItem(item);
        }
        this._status.label.text = `Updated ${new Date().toLocaleTimeString()}`;
    }

    _showFailure(message) {
        this._box.destroy_all_children();
        this._box.add_child(new St.Label({text: 'Claude ⚠', style_class: 'claude-usage-none', y_align: Clutter.ActorAlign.CENTER}));
        this._status.label.text = message;
    }

    destroy() {
        this._cancellable?.cancel();
        this._proc?.force_exit();
        super.destroy();
    }
});

export default class ClaudeUsageExtension extends Extension {
    enable() {
        this._indicator = new UsageIndicator();
        Main.panel.addToStatusArea(this.uuid, this._indicator);
        this._indicator.refresh(false);
        this._timer = GLib.timeout_add_seconds(GLib.PRIORITY_DEFAULT, POLL_SECONDS, () => {
            this._indicator.refresh(false);
            return GLib.SOURCE_CONTINUE;
        });
    }

    disable() {
        if (this._timer) {
            GLib.source_remove(this._timer);
            this._timer = null;
        }
        this._indicator?.destroy();
        this._indicator = null;
    }
}
