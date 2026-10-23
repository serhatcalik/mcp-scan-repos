package example;

import jakarta.persistence.Entity;
import jakarta.persistence.Id;

@Entity
public class Appointment {
    @Id
    private Long id;
}
