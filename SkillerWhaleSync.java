// TODO

import java.nio.file.*;
import static java.nio.file.LinkOption.*;
import java.nio.file.attribute.*;
import java.io.*;
import java.util.*;
import java.util.concurrent.TimeUnit;
import java.net.http.*;
import java.net.URI;

public class SkillerWhaleSync {
    private final class RescanException extends Exception { }

    private final WatchService watcher;
    private final Map<WatchKey,Path> keys;

    private String attendanceId = "unknown";

    private void post(String uri, String data) throws IOException, InterruptedException {
        System.out.println(uri);
        HttpClient client = HttpClient.newHttpClient();
        HttpRequest request = HttpRequest.newBuilder()
                .uri(URI.create(uri))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(data))
                .build();

        HttpResponse<?> response = client.send(request, HttpResponse.BodyHandlers.discarding());
        if (response.statusCode() != 200) {
            System.err.println("Unsuccessful "+response+" posting to "+uri);
        } else {
            System.err.println(response);
        }
    }

    private void postToEndpoint(String endpoint, String data) throws IOException, InterruptedException {
        post("https://train.skillerwhale.com/attendances/"+attendanceId+"/"+endpoint, data);
    }

    private void ping() throws IOException, InterruptedException {
        postToEndpoint("pings", "");
    }

    public static String quote(String string) {
        if (string == null || string.length() == 0) {
            return "\"\"";
        }

        char         c = 0;
        int          i;
        int          len = string.length();
        StringBuilder sb = new StringBuilder(len + 4);
        String       t;

        sb.append('"');
        for (i = 0; i < len; i += 1) {
            c = string.charAt(i);
            switch (c) {
            case '\\':
            case '"':
                sb.append('\\');
                sb.append(c);
                break;
            case '/':
                sb.append('\\');
                sb.append(c);
                break;
            case '\b':
                sb.append("\\b");
                break;
            case '\t':
                sb.append("\\t");
                break;
            case '\n':
                sb.append("\\n");
                break;
            case '\f':
                sb.append("\\f");
                break;
            case '\r':
               sb.append("\\r");
               break;
            default:
                if (c < ' ') {
                    t = "000" + Integer.toHexString(c);
                    sb.append("\\u" + t.substring(t.length() - 4));
                } else {
                    sb.append(c);
                }
            }
        }
        sb.append('"');
        return sb.toString();
    }

    private void postFile(Path path, String contents) throws IOException, InterruptedException {
        postToEndpoint("file_snapshots", "{ relative_path: \""+ quote(path.toString()) +"\", contents: \""+quote(contents)+"\" }");
    }

    private void register(Path dir) throws IOException {
        WatchKey key = dir.register(watcher, StandardWatchEventKinds.ENTRY_CREATE, StandardWatchEventKinds.ENTRY_DELETE, StandardWatchEventKinds.ENTRY_MODIFY);
        if (true) {
            Path prev = keys.get(key);
            if (prev == null) {
                System.out.format("register: %s\n", dir);
            } else {
                if (!dir.equals(prev)) {
                    System.out.format("update: %s -> %s\n", prev, dir);
                }
            }
        }
        keys.put(key, dir);
    }

    private void readAttendanceId() {
        try {
            this.attendanceId = Files.readString(Path.of("attendance_id")).replaceAll("\\s","");
        }
        catch (IOException e) {
        }
    }

    private void registerAll(final Path start) throws IOException {
        // register directory and sub-directories
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

    void checkFile(Path file) throws InterruptedException, IOException {
        if (file.endsWith("attendance_id")) {
            readAttendanceId();
        } else {
            System.out.println("GO!");
            postFile(file, Files.readString(file));
        }
    }

    /**
     * Process all events for keys queued to the watcher
     */
    boolean processEvent() throws RescanException, InterruptedException, IOException {
        WatchKey key = watcher.poll(1, TimeUnit.SECONDS);
        if (key == null) {
            return false;
        }
        Path dir = keys.get(key);
        assert(dir != null);

        for (WatchEvent<?> event : key.pollEvents()) {
            Path child = dir.resolve(event.context().toString());
            System.out.format("%s: %s\n", event.kind().name(), child);

            switch (event.kind().name()) {
                case "OVERFLOW" -> throw new RescanException();

                case "ENTRY_CREATE" -> {
                    if (Files.isDirectory(child, NOFOLLOW_LINKS)) {
                        registerAll(child);
                    } else if (Files.isRegularFile(child, NOFOLLOW_LINKS)) {
                        checkFile(child);
                    }
                }
                case "ENTRY_DELETE" -> {

                }
                case "ENTRY_MODIFY" -> {
                    if (Files.isRegularFile(child, NOFOLLOW_LINKS)) {
                        checkFile(child);
                    }
                }
            }
        }

        if (!key.reset()) {
            keys.remove(key);
        }

        return true;
    }

    /**
     * Creates a WatchService and registers the given directory
     */
    SkillerWhaleSync(Path dir) throws IOException {
        this.watcher = FileSystems.getDefault().newWatchService();
        this.keys = new HashMap<WatchKey,Path>();
        registerAll(dir);
        readAttendanceId();
    }

    public static void main(String[] args) throws IOException, InterruptedException {
        while (true) {
            var sync = new SkillerWhaleSync(Paths.get(args[0]));
            try {
                while(true) {
                    if (!sync.processEvent()) {
                        sync.ping();
                    }
                }
            }
            catch (RescanException e) {
            }
        }
    }
}
