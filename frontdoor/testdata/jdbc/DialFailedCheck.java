// A REAL JDBC CLIENT'S VIEW OF A REQUEST WHOSE BACKEND COULD NOT BE OPENED.
//
// The Go cells beside this one prove the frame is what the front door says it
// is, and that pgx keeps the session across it. This program answers the
// question only a second, independently written driver can answer: whether the
// chosen SQLSTATE leaves a JDBC Connection usable, or whether pgjdbc decides
// the connection is broken and every later statement on it fails.
//
// Class 08 is the code that reads best and behaves worst: pgjdbc maps several
// class 08 states onto a closed connection. This program is what turns that
// from a belief into a measurement.
//
// It drives the two protocols separately, because they recover differently.
// preferQueryMode=simple sends a Query message and expects an error followed by
// ReadyForQuery. The default mode sends Parse, Bind, Describe, Execute and Sync
// and expects the server to discard through that Sync.
//
// The armed statement carries a marker the test fixture recognises, so this
// program needs no way to reach into the server to switch the fault on.

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.Properties;

import org.postgresql.util.PSQLException;
import org.postgresql.util.ServerErrorMessage;

public class DialFailedCheck {

    private static final String MARKER = "/*dial-fault*/";

    public static void main(String[] args) throws Exception {
        String url = args[0];
        String user = args[1];
        String password = args[2];

        run("simple", url, user, password, true);
        run("extended", url, user, password, false);
        System.out.println("DONE");
    }

    private static void run(String label, String url, String user, String password,
                            boolean simpleProtocol) throws Exception {
        Properties p = new Properties();
        p.setProperty("user", user);
        p.setProperty("password", password);
        p.setProperty("ssl", "true");
        p.setProperty("sslmode", "require");
        // The listener under test is self-signed; verification is the cell's
        // subject nowhere, so it is switched off explicitly rather than by a
        // truststore the test would have to build.
        p.setProperty("sslfactory", "org.postgresql.ssl.NonValidatingFactory");
        if (simpleProtocol) {
            p.setProperty("preferQueryMode", "simple");
        }

        try (Connection c = DriverManager.getConnection(url, p)) {
            emit(label, "connected", "true");

            try {
                if (simpleProtocol) {
                    try (Statement s = c.createStatement()) {
                        s.executeQuery("SELECT 1 " + MARKER);
                    }
                } else {
                    try (PreparedStatement s = c.prepareStatement("SELECT ?::int AS n " + MARKER)) {
                        s.setInt(1, 7);
                        s.executeQuery();
                    }
                }
                emit(label, "armed_failed", "false");
                return;
            } catch (SQLException e) {
                emit(label, "armed_failed", "true");
                emit(label, "sqlstate", e.getSQLState());
                // The driver's own getMessage() prefixes the severity and
                // appends DETAIL and HINT, so it is NOT the server's literal.
                // Both are reported: the literal is the thing the contract
                // fixes, and the rendered string is what a user would see.
                emit(label, "driver_message", e.getMessage());
                if (e instanceof PSQLException) {
                    ServerErrorMessage sem = ((PSQLException) e).getServerErrorMessage();
                    if (sem != null) {
                        emit(label, "severity", sem.getSeverity());
                        emit(label, "message", sem.getMessage());
                        emit(label, "detail", String.valueOf(sem.getDetail()));
                        emit(label, "hint", String.valueOf(sem.getHint()));
                    } else {
                        // No server error message means pgjdbc did not parse
                        // this as an ErrorResponse at all — it treated it as a
                        // transport failure, which is the loudest possible
                        // failure of the contract.
                        emit(label, "severity", "NONE");
                    }
                }
            }

            // THE QUESTION THIS PROGRAM EXISTS TO ANSWER.
            emit(label, "closed", String.valueOf(c.isClosed()));
            emit(label, "valid", String.valueOf(c.isValid(5)));

            // And the session must still do real work against the real target,
            // in the same protocol it just failed in.
            if (simpleProtocol) {
                try (Statement s = c.createStatement();
                     ResultSet rs = s.executeQuery("SELECT 40 + 2 AS n")) {
                    rs.next();
                    emit(label, "after", String.valueOf(rs.getInt(1)));
                }
            } else {
                try (PreparedStatement s = c.prepareStatement("SELECT ?::int AS n")) {
                    s.setInt(1, 42);
                    try (ResultSet rs = s.executeQuery()) {
                        rs.next();
                        emit(label, "after", String.valueOf(rs.getInt(1)));
                    }
                }
            }
        }
    }

    private static void emit(String label, String key, String value) {
        System.out.println(label + "." + key + "=" + String.valueOf(value).replace('\n', ' '));
    }
}
