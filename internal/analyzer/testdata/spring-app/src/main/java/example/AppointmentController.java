package example;

import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
public class AppointmentController {
    @GetMapping("/appointments")
    public String list() {
        return "[]";
    }
}
