package app.pifoto

import android.content.Context
import android.content.SharedPreferences

/** All app settings live in one private SharedPreferences file. */
class Prefs(context: Context) {
    private val p: SharedPreferences =
        context.applicationContext.getSharedPreferences("pifoto", Context.MODE_PRIVATE)

    var server: String
        get() = p.getString("server", DEFAULT_SERVER)!!
        set(v) = p.edit().putString("server", normalizeServer(v)).apply()

    /**
     * Address that reaches the Pi from anywhere, told by the server (/api/info). Until the app has
     * heard from the server, the built-in public address, so a fresh install works away from home.
     * Always https: the app sends its session here without checking first, so only an address whose
     * TLS certificate proves it is the Pi will do. Anything else (an old Tailscale http:// address
     * from earlier versions, or one a fake server suggested) is ignored.
     */
    var remote: String
        get() = p.getString("remote", "")!!.takeIf { it.startsWith("https://") } ?: PUBLIC_SERVER
        set(v) {
            val s = if (v.isBlank()) "" else normalizeServer(v)
            if (s.isEmpty() || s.startsWith("https://")) p.edit().putString("remote", s).apply()
        }

    /** Value of the pf_s session cookie, or "" when logged out. */
    var session: String
        get() = p.getString("session", "")!!
        set(v) = p.edit().putString("session", v).apply()

    /** Name of the person logged in on this device (as the server spells it); kept after logging out
     *  to fill in the next login. "" = not known yet (e.g. logged in before family accounts). */
    var name: String
        get() = p.getString("name", "")!!
        set(v) = p.edit().putString("name", v.trim()).apply()

    /** The server's pf_d device cookie from the last login: with it, strangers guessing this
     *  person's password from the internet can't lock this device out of logging in again. */
    var device: String
        get() = p.getString("device", "")!!
        set(v) = p.edit().putString("device", v).apply()

    var backupEnabled: Boolean
        get() = p.getBoolean("backup", false)
        set(v) = p.edit().putBoolean("backup", v).apply()

    var wifiOnly: Boolean
        get() = p.getBoolean("wifiOnly", true)
        set(v) = p.edit().putBoolean("wifiOnly", v).apply()

    var chargingOnly: Boolean
        get() = p.getBoolean("chargingOnly", false)
        set(v) = p.edit().putBoolean("chargingOnly", v).apply()

    /** Per storage volume: MediaStore GENERATION_ADDED of the last item handled; newer ones are pending.
     *  Unknown volumes (e.g. an SD card inserted later) start at 0 = back up everything on them. */
    fun watermark(volume: String): Long = p.getLong("wm_$volume", 0L)

    fun setWatermark(volume: String, gen: Long) {
        p.edit().putLong("wm_$volume", gen).commit() // commit: must survive the process being killed
    }

    /** MediaStore.getVersion per volume; when it changes, generations were reset (DB rebuild). */
    fun mediaVersion(volume: String): String? = p.getString("ver_$volume", null)
    fun setMediaVersion(volume: String, v: String) { p.edit().putString("ver_$volume", v).commit() }

    /** IDs above the watermark that are already handled (uploaded past a still-pending file). */
    fun doneAbove(volume: String): Set<Long> =
        p.getStringSet("done_$volume", emptySet())!!.mapNotNull { it.toLongOrNull() }.toSet()
    fun setDoneAbove(volume: String, ids: Set<Long>) {
        p.edit().putStringSet("done_$volume", ids.map { it.toString() }.toSet()).commit()
    }

    var uploadedTotal: Long
        get() = p.getLong("uploadedTotal", 0L)
        set(v) = p.edit().putLong("uploadedTotal", v).apply()

    var lastRun: Long
        get() = p.getLong("lastRun", 0L)
        set(v) = p.edit().putLong("lastRun", v).apply()

    var lastStatus: String
        get() = p.getString("lastStatus", "")!!
        set(v) = p.edit().putString("lastStatus", v).apply()

    /** Summary of the last finished pass ("3 backed up · 1 already there"); only written by the worker. */
    var lastSummary: String
        get() = p.getString("lastSummary", "")!!
        set(v) = p.edit().putString("lastSummary", v).apply()

    /** Why backup is waiting: one of the BackupStatus.PROBLEM_* codes, "" when the last pass went fine. */
    var problem: String
        get() = p.getString("problem", "")!!
        set(v) = p.edit().putString("problem", v).apply()

    val loggedIn: Boolean get() = session.isNotEmpty()

    // ---------- app updates (Updater) ----------
    /** When the app last asked the Pi for a newer version (ms). */
    var updateChecked: Long
        get() = p.getLong("updateChecked", 0)
        set(v) = p.edit().putLong("updateChecked", v).apply()
    /** A versionCode whose install failed for good (another signing key, too old an Android): skipped. */
    var updateFailed: Long
        get() = p.getLong("updateFailed", 0)
        set(v) = p.edit().putLong("updateFailed", v).apply()
    /** An update is downloaded and waits until nobody is using the app. */
    var updateWaiting: Boolean
        get() = p.getBoolean("updateWaiting", false)
        set(v) = p.edit().putBoolean("updateWaiting", v).apply()
    /** That update ("code|size|sha256|name|notes"), so it can be installed without asking the Pi again:
     *  right after the app is left there may be no network for it. */
    var updateRelease: Api.Release?
        get() = p.getString("updateRelease", "")!!.split('|', limit = 5).takeIf { it.size == 5 }?.let {
            val code = it[0].toLongOrNull(); val size = it[1].toLongOrNull()
            if (code == null || size == null) null else Api.Release(code, it[3], it[4], size, it[2])
        }
        set(v) = p.edit().putString("updateRelease", if (v == null) "" else
            "${v.code}|${v.size}|${v.sha256}|${v.name.replace("|", "")}|${v.notes}").apply()
    /** Android wants a tap to install the update (no permission to update silently yet): the next
     *  time the app is opened, it shows Android's install screen. */
    var updateNeedsTap: Boolean
        get() = p.getBoolean("updateNeedsTap", false)
        set(v) = p.edit().putBoolean("updateNeedsTap", v).apply()
    /** When someone last said no on that screen (ms): not asked again in the app for a day. */
    var updateDeclined: Long
        get() = p.getLong("updateDeclined", 0)
        set(v) = p.edit().putLong("updateDeclined", v).apply()
    /** The version being installed and its notes, for the "updated" notification the new version shows. */
    var updateName: String
        get() = p.getString("updateName", "")!!
        set(v) = p.edit().putString("updateName", v).commit().let {}
    var updateNotes: String
        get() = p.getString("updateNotes", "")!!
        set(v) = p.edit().putString("updateNotes", v).commit().let {}

    /**
     * The server no longer accepts our session (logged out elsewhere, password reset, person removed).
     * Forget it, and remember why so the login screen and the backup status can say so.
     * Only if [session] is still the current one: a newer login must not be thrown away.
     */
    fun sessionEnded(session: String): Boolean {
        if (session.isEmpty() || this.session != session) return false
        p.edit().putString("session", "").putString("problem", BackupStatus.PROBLEM_AUTH).commit()
        return true
    }

    companion object {
        const val DEFAULT_SERVER = "http://raspberrypi.fritz.box"
        /**
         * The Pi's public address (Tailscale Funnel): works on any internet connection, no Tailscale app
         * needed. Set per build (pifoto.publicServer in android/local.properties); the server's own
         * -remote-url replaces it once the app has talked to the server.
         */
        val PUBLIC_SERVER: String = BuildConfig.PUBLIC_SERVER

        /**
         * Without a scheme: https for Tailscale names (*.ts.net; Funnel only speaks https, and the app
         * allows plain http only for the home address), else http (a typed home address or IP).
         */
        fun normalizeServer(raw: String): String {
            val s = raw.trim().trimEnd('/')
            if (s.isEmpty()) return DEFAULT_SERVER
            val scheme = s.substringBefore("://", "").lowercase()
            if (scheme == "http" || scheme == "https") return scheme + s.substring(scheme.length)
            val host = s.substringBefore('/').substringBefore(':').lowercase()
            return (if (host.endsWith(".ts.net")) "https://" else "http://") + s
        }
    }
}
