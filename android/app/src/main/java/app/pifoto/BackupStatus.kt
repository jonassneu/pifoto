package app.pifoto

import android.content.Context
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.concurrent.atomic.AtomicReference
import org.json.JSONObject

/**
 * One view of "how is backup doing", shared by the settings screen and the web app's bridge
 * (window.PiFotoApp.backupStatus()). Cheap and thread-safe: reads SharedPreferences (in memory)
 * and the live state of the running pass; never touches the network or MediaStore.
 */
object BackupStatus {
    const val PROBLEM_NET = "net"      // the Pi didn't answer; a retry is scheduled
    const val PROBLEM_READ = "read"    // a file couldn't be read (e.g. SD card removed); retry scheduled
    const val PROBLEM_ERROR = "error"  // anything else; retry scheduled
    const val PROBLEM_AUTH = "auth"    // the server ended the session mid-run: log in again
    const val PROBLEM_FULL = "full"    // the storage limit on the Pi is reached: waits for more space
    const val PROBLEM_PERM = "perm"    // not stored: derived from the current permission state

    /** The pass that is running right now (BackupWorker runs in the app's process). null = none. */
    data class Live(
        val startedAt: Long,
        val total: Int = 0,         // files to upload in this pass so far (grows if photos arrive meanwhile)
        val done: Int = 0,          // files finished (uploaded, already there, or skipped)
        val file: String = "",      // file being uploaded
        val filePct: Int = -1,      // its progress, -1 = unknown
        val bytesTotal: Long = 0,   // MediaStore sizes of all files in the pass
        val bytesDone: Long = 0,    // finished files + bytes sent of the current one
    ) {
        val left get() = (total - done).coerceAtLeast(0)
        /** Overall fraction of the pass, by bytes when sizes are known, else by files. -1 = unknown. */
        val progress: Double get() = when {
            total <= 0 -> -1.0
            bytesTotal > 0 -> (bytesDone.toDouble() / bytesTotal).coerceIn(0.0, 1.0)
            else -> ((done + filePct.coerceAtLeast(0) / 100.0) / total).coerceIn(0.0, 1.0)
        }
    }

    private val live = AtomicReference<Live?>(null)

    fun live(): Live? = live.get()
    fun start(now: Long) = live.set(Live(startedAt = now))
    fun update(f: (Live) -> Live) { live.updateAndGet { it?.let(f) } }
    fun clear() = live.set(null)

    enum class State(val json: String) { OFF("off"), OK("ok"), RUN("run"), PAUSED("paused") }

    class Snapshot(
        val state: State, val problem: String, val live: Live?,
        val uploadedTotal: Long, val lastRun: Long, val wifiOnly: Boolean, val chargingOnly: Boolean,
        val lastSummary: String, val device: String,
    ) {
        val left get() = if (state == State.RUN) live?.left ?: 0 else 0
        val progress get() = if (state == State.RUN) live?.progress ?: -1.0 else -1.0
        /** "IMG_5032.jpg · 35%" while a file is uploading, else "". */
        val file get() = live?.takeIf { state == State.RUN && it.file.isNotEmpty() }
            ?.let { if (it.filePct >= 0) "${it.file} · ${it.filePct}%" else it.file } ?: ""
        val reason get() = problemText(problem)
    }

    fun device(ctx: Context) = if (ctx.resources.configuration.smallestScreenWidthDp >= 600) "tablet" else "phone"

    fun snapshot(ctx: Context, device: String = device(ctx)): Snapshot {
        val p = Prefs(ctx)
        val l = live.get()
        var problem = ""
        val state = when {
            !p.backupEnabled -> State.OFF
            !p.loggedIn -> if (p.problem == PROBLEM_AUTH) State.PAUSED.also { problem = PROBLEM_AUTH } else State.OFF
            !Backup.hasMediaPermission(ctx) -> State.PAUSED.also { problem = PROBLEM_PERM }
            l != null -> if (l.left > 0) State.RUN else State.OK // checking, or between rounds
            p.problem.isNotEmpty() -> State.PAUSED.also { problem = p.problem }
            else -> State.OK
        }
        return Snapshot(state, problem, l, p.uploadedTotal, p.lastRun, p.wifiOnly, p.chargingOnly, p.lastSummary, device)
    }

    fun problemText(code: String) = when (code) {
        PROBLEM_NET -> "Can't reach the Pi — will retry"
        PROBLEM_READ -> "Couldn't read a photo — will retry"
        PROBLEM_AUTH -> "Log in again to keep backing up"
        PROBLEM_PERM -> "Photo access permission is missing"
        PROBLEM_FULL -> "Your storage on the Pi is full — ask an admin for more"
        "" -> ""
        else -> "Something went wrong — will retry"
    }

    /** The bridge contract: see MainActivity.Bridge.backupStatus. */
    fun json(ctx: Context, device: String): String {
        val s = snapshot(ctx, device)
        val (title, detail) = when (s.state) {
            State.OFF -> "Backup is off" to "Turn it on to upload new photos automatically"
            State.RUN -> "Backing up this ${s.device}" to "${s.left} ${if (s.left == 1) "photo" else "photos"} left"
            State.PAUSED -> "Backup paused" to s.reason
            State.OK -> "Up to date" to lastCheck(s.lastRun)
        }
        return JSONObject()
            .put("enabled", s.state != State.OFF)
            .put("state", s.state.json)
            .put("title", title)
            .put("detail", detail)
            .put("file", s.file)
            .put("progress", s.progress)
            .put("left", s.left)
            .put("uploadedTotal", s.uploadedTotal)
            .put("device", s.device)
            .put("wifiOnly", s.wifiOnly)
            .put("chargingOnly", s.chargingOnly)
            .put("lastRun", s.lastRun)
            .toString()
    }

    fun lastCheck(lastRun: Long) = if (lastRun > 0) "Last check ${ago(lastRun)}" else "Not checked yet"

    /** "just now", "4 minutes ago", "yesterday", "on Oct 3" — English, like the rest of the UI. */
    fun ago(t: Long, now: Long = System.currentTimeMillis()): String {
        val d = now - t
        fun n(v: Long, unit: String) = "$v $unit${if (v == 1L) "" else "s"} ago"
        return when {
            d < 60_000 -> "just now"
            d < 3_600_000 -> n(d / 60_000, "minute")
            d < 86_400_000 -> n(d / 3_600_000, "hour")
            d < 2 * 86_400_000 -> "yesterday"
            d < 7 * 86_400_000L -> n(d / 86_400_000, "day")
            else -> "on " + SimpleDateFormat("MMM d", Locale.US).format(Date(t))
        }
    }

    fun count(n: Long) = String.format(Locale.US, "%,d", n)
}
