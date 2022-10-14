package com.skillerwhale.sync;

import java.nio.ByteBuffer;
import java.nio.CharBuffer;
import java.nio.charset.Charset;
import java.nio.charset.CharsetDecoder;
import java.nio.charset.CoderResult;
import java.nio.file.*;
import static java.nio.file.LinkOption.*;
import java.nio.file.attribute.*;
import java.io.*;
import java.util.*;
import java.util.regex.*;
import java.util.logging.*;
import java.util.concurrent.TimeUnit;
import java.net.http.*;
import java.net.URI;

public class SkillerWhaleSync {
    /* Logging */
    {
        System.setProperty("java.util.logging.SimpleFormatter.format", "%1$tF %1$tT %4$s %2$s %5$s%6$s%n");
    }
    private static final Logger LOG = Logger.getLogger( SkillerWhaleSync.class.getName() );

    /* Performance tuning */
    public static final int MAX_UPLOAD_BYTES = 10_000_000;
    public static final int SEND_AFTER_MILLIS = 250;
    public static final int WAIT_POLL_MILLIS = 500;
    public static final int PING_EVERY_MILLIS = 2000; // multiple of WAIT_POLL_MILLIS

    /* Parsed configuration */
    private final String serverUrl;
    private final Path attendanceIdFile;
    private final Path base;
    private final String[] ignoreDirs;
    private final String[] watchedExts;

    /* Runtime */
    private final WatchService watcher;
    private final Map<WatchKey,Path> keys;
    private final Map<Path,Long> updatedFiles;
    private String attendanceId;
    private boolean attendanceIdValid = false;

    private void postJSON(String uri, String data) throws IOException, InterruptedException {
        // Don't forget `java -Djdk.httpclient.HttpClient.log=headers,requests` for debugging

        HttpClient client = HttpClient.newHttpClient();
        HttpRequest request = HttpRequest.newBuilder()
                .uri(URI.create(uri))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(data))
                .build();

        HttpResponse<?> response = client.send(request, HttpResponse.BodyHandlers.ofString());
        if (response.statusCode() >= 200 && response.statusCode() < 300) {
            return;
        }

        if (response.statusCode() >= 400 && response.statusCode() < 500) {
            attendanceIdValid = false;
            if (attendanceIdFile == null) {
                throw new IOException("No valid attendance_id, set ATTENDANCE_ID_FILE or a correct ATTENDANCE_ID");
            }
        }

        // Ignore 500 errors
    }

    private boolean postToEndpoint(String endpoint, String data) throws IOException, InterruptedException {
        if (!attendanceIdValid)
            return false;
        postJSON(serverUrl+"attendances/"+attendanceId+"/"+endpoint, data);
        return true;
    }

    private boolean ping() throws IOException, InterruptedException {
        return postToEndpoint("pings", "");
    }
/*
    private String getEncodingClue(byte[] data) {
        if (data.length > 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf) {
            return "UTF-8";
        }
        String asciiHeader = new String(data, 0, 1000, "ASCII");
        Matcher m = Pattern.compile("^[ \t\f]*#.*?coding[:=][ \t]*([-_.a-zA-Z0-9]+)", Pattern.MULTILINE).matcher(asciiHeader);
        if (m.matches()) {
            return m.group(1);
        }
        return null;
    }
*/
    /* Assumptions for a random file read off the disc: 1) it's UTF-8, or 2)
     * we still want to see it, so interpret (badly) as ISO-8859-1.
     */
    public static String bytesToString(byte[] data) {
        //String encodingClue = getEncodingClue();
        for (String tryCharset : new String[]{"UTF8","ISO-8859-1"}) {
            CharBuffer     out     = CharBuffer.allocate(data.length);
            CharsetDecoder decoder = Charset.forName(tryCharset).newDecoder();
            if (decoder.decode(ByteBuffer.wrap(data), out, false) == CoderResult.UNDERFLOW) {
                out.flip();
                return out.toString();
            }
        }
        return null;
    }

    public static String jsonStringQuote(String string) {
        StringBuilder sb = new StringBuilder("\"");
        for (char c : string.toCharArray())
            sb.append(switch (c) {
                case '\\', '"', '/' -> "\\"+c;
                case '\b' -> "\\b";
                case '\t' -> "\\t";
                case '\n' -> "\\n";
                case '\f' -> "\\f";
                case '\r' -> "\\r";
                default -> c < ' ' ? String.format("\\u%04x", c) : c;
            });
        return sb.append('"').toString();
    }

    private boolean postFile(Path path, byte[] contents) throws IOException, InterruptedException {
        return postToEndpoint(
            "file_snapshots",
            "{"+
            "\"relative_path\": "+ jsonStringQuote(base.relativize(path).toString()) +
            ","+
            "\"contents\": "+jsonStringQuote(bytesToString(contents))
            +" }"
        );
    }

    private void register(Path dir) throws IOException {
        WatchKey key = dir.register(watcher, StandardWatchEventKinds.ENTRY_CREATE, StandardWatchEventKinds.ENTRY_DELETE, StandardWatchEventKinds.ENTRY_MODIFY);
        /*Path prev = keys.get(key);
        if (prev == null) {
            System.out.format("register: %s\n", dir);
        } else {
            if (!dir.equals(prev)) {
                System.out.format("update: %s -> %s\n", prev, dir);
            }
        }*/
        keys.put(key, dir);
    }

    private void readAttendanceId() {
        try {
            if (Files.size(attendanceIdFile) < 100) {
                String newAttendanceId = Files.readString(attendanceIdFile).replaceAll("\\s","");
                if (!newAttendanceId.equals(attendanceId)) {
                    attendanceId = newAttendanceId;
                    attendanceIdValid = true;
                    ping();
                    if (attendanceIdValid == true) {
                        LOG.log(Level.INFO, "valid   {0}", attendanceId);
                    } else {
                        LOG.log(Level.INFO, "invalid {0}", attendanceId);
                    }
                }
            }
        }
        catch (IOException | InterruptedException e) {
            LOG.log(Level.WARNING, "", e);
        }
    }

    private void registerAll(final Path start) throws IOException {
        Files.walkFileTree(start, new SimpleFileVisitor<Path>() {
            @Override
            public FileVisitResult preVisitDirectory(Path dir, BasicFileAttributes attrs)
                throws IOException
            {
                register(dir);
                return FileVisitResult.CONTINUE;
            }
        });
    }

    void readAndPostFile(Path file) throws InterruptedException, IOException {
        try (FileInputStream in = new FileInputStream(file.toString())) {
            var len = in.available();
            if (len < MAX_UPLOAD_BYTES) {
                var buffer = new byte[len];
                if (in.read(buffer) != len) {
                    throw new IOException("Couldn't read all of "+file+" in one go");
                }
                if (postFile(file, buffer)) {
                    LOG.log(Level.INFO,    "upload {0}", file);
                } else {
                    LOG.log(Level.WARNING, "ignore {0}", file);
                }
            } else {
                    LOG.log(Level.WARNING, "too large {0}", file);
            }
        }
    }

    void postUpdatedFiles() throws IOException, InterruptedException {
        for (var e : updatedFiles.entrySet()) {
            if (System.currentTimeMillis() - e.getValue() > SEND_AFTER_MILLIS) {
                readAndPostFile(e.getKey());
                updatedFiles.remove(e.getKey());
            }
        }
    }

    void processEvent(WatchEvent<?> event, Path child) throws IOException {
        try {
            switch (event.kind().name()) {
                case "OVERFLOW" -> {
                    LOG.log(Level.WARNING, "overflow, might have missed something");
                }

                case "ENTRY_CREATE" -> {
                    if (Files.isDirectory(child, NOFOLLOW_LINKS)) {
                        registerAll(child);
                    } else if (Files.isRegularFile(child, NOFOLLOW_LINKS)) {
                        //checkFile(child);
                        updatedFiles.put(child, System.currentTimeMillis());
                    }
                }
                case "ENTRY_DELETE" -> {
                    // Server doesn't care about deletes?
                }
                case "ENTRY_MODIFY" -> {
                    if (Files.isRegularFile(child, NOFOLLOW_LINKS)) {
                        //checkFile(child);
                        updatedFiles.put(child, System.currentTimeMillis());
                    }
                }
            }
        }
        catch (java.nio.file.NoSuchFileException nsf) {
            // Always a possible race between notification & file being deleted
            //
            // Other IOExceptions would seem unusual, should probably let the program die
        }
    }

    /**
     * Process all events for keys queued to the watcher
     */
    boolean waitAndProcessEvents() throws InterruptedException, IOException {
        WatchKey key = watcher.poll(WAIT_POLL_MILLIS, TimeUnit.MILLISECONDS);
        if (key == null) {
            return false;
        }
        Path dir = keys.get(key);
        assert(dir != null);

        for (WatchEvent<?> event : key.pollEvents()) {
            Path child = dir.resolve(event.context().toString()).toAbsolutePath();
            boolean matchExt = Arrays.stream(watchedExts).
                anyMatch(s -> child.toString().endsWith(s));
            boolean matchIgnore = Arrays.stream(ignoreDirs).
                anyMatch(s -> child.toString().contains(s+"/"));

            // additional paranoia
            if (Files.isRegularFile(child, LinkOption.NOFOLLOW_LINKS) && child.startsWith(base) && matchExt && !matchIgnore)
                processEvent(event, child);
        }

        if (!key.reset()) {
            keys.remove(key);
        }

        return true;
    }

    private static String getenvWithDefault(String name, String defaultV) {
        String v = System.getenv(name);
        return v != null ? v : defaultV;
    }

    private static String[] getenvAndSplit(String name) {
        String v = getenvWithDefault(name, null);
        if (v == null) {
            return new String[0];
        }
        // deal with simple JSON array
        return v.replaceAll("[\\[\\]\"]","").split(" +");
    }

    SkillerWhaleSync(String attendanceId, Path attendanceIdFile, String serverUrl, Path base, String[] watchedExts, String[] ignoreDirs) throws IOException {
        this.attendanceId = attendanceId;
        this.attendanceIdFile = attendanceIdFile;
        this.serverUrl = serverUrl;
        this.base = base;
        this.watchedExts = watchedExts;
        this.ignoreDirs = ignoreDirs;

        this.watcher = FileSystems.getDefault().newWatchService();
        this.keys = new HashMap<WatchKey,Path>();
        this.updatedFiles = new HashMap<Path,Long>();

        registerAll(base);
        readAttendanceId();
    }

    public static SkillerWhaleSync createFromEnvironment() throws IOException {
        return new SkillerWhaleSync(
            getenvWithDefault("ATTENDANCE_ID", null),
            Paths.get(getenvWithDefault("ATTENDANCE_ID_FILE", "attendance_id")),
            getenvWithDefault("SERVER_URL", "https://train.skillerwhale.com/"),
            Paths.get(getenvWithDefault("WATCHER_BASE_PATH", ".")).normalize().toAbsolutePath(),
            getenvAndSplit("WATCHED_EXTS"),
            getenvAndSplit("IGNORE_DIRS")
        );
    }

    public static void main(String[] args) throws IOException, InterruptedException {
        var sync = createFromEnvironment();
        long lastPing = 0;
        while(true) {
            sync.readAttendanceId();

            long now = System.currentTimeMillis();
            if (lastPing + PING_EVERY_MILLIS < now) {
                sync.ping();
                lastPing = now;
            }

            sync.postUpdatedFiles();
            sync.waitAndProcessEvents();
        }
    }
}
